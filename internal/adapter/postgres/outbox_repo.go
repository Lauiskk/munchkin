package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Lauiskk/munchkin/internal/app/outbox"
	"github.com/Lauiskk/munchkin/internal/domain/event"
)

// outboxRow é a linha da tabela de eventos a publicar.
type outboxRow struct {
	EventID       uuid.UUID `gorm:"column:event_id;primaryKey"`
	EventType     string    `gorm:"column:event_type"`
	AggregateType string    `gorm:"column:aggregate_type"`
	AggregateID   uuid.UUID `gorm:"column:aggregate_id"`
	Version       int       `gorm:"column:version"`
	CorrelationID *string   `gorm:"column:correlation_id"`
	CausationID   *string   `gorm:"column:causation_id"`
	Payload       []byte    `gorm:"column:payload"`
	OccurredAt    time.Time `gorm:"column:occurred_at"`

	// Controle de publicação. Só a reivindicação lê e escreve estas colunas; a
	// gravação do evento não as toca, e o gatilho de imutabilidade protege
	// apenas o payload — o instantâneo é imutável, o controle não é.
	Attempts int `gorm:"column:attempts"`
}

func (outboxRow) TableName() string { return "outbox_events" }

// OutboxRepository grava eventos para publicação posterior.
type OutboxRepository struct{ db *Database }

// NewOutboxRepository monta o repositório.
func NewOutboxRepository(db *Database) *OutboxRepository { return &OutboxRepository{db: db} }

// Enqueue grava o evento na outbox.
//
// Exige transação aberta, e a exigência é a garantia central da publicação: o
// evento tem de ser confirmado JUNTO com a mudança que o originou. Gravá-lo fora
// da transação permitiria que ele existisse sem o fato que descreve — ou que o
// fato existisse sem o evento.
//
// Publicar é trabalho de outro worker, depois do commit. Nenhum caminho daqui
// toca a fila.
func (r *OutboxRepository) Enqueue(ctx context.Context, env event.Envelope) error {
	if !InTransaction(ctx) {
		return fmt.Errorf("%w: Enqueue de evento", ErrNoTransaction)
	}

	payload, err := json.Marshal(env.Payload())
	if err != nil {
		return fmt.Errorf("serialização do evento %s: %w", env.Type(), err)
	}

	row := outboxRow{
		EventID:       uuid.UUID(env.ID()),
		EventType:     string(env.Type()),
		AggregateType: string(env.AggregateType()),
		AggregateID:   uuid.UUID(env.AggregateID()),
		Version:       env.Version(),
		Payload:       payload,
		OccurredAt:    env.OccurredAt(),
	}
	if id := env.CorrelationID(); id != "" {
		row.CorrelationID = &id
	}
	if id := env.CausationID(); id != "" {
		row.CausationID = &id
	}

	if err := r.db.Session(ctx).Create(&row).Error; err != nil {
		return classify(err)
	}
	return nil
}

// CountPending conta os eventos ainda não publicados de um agregado.
func (r *OutboxRepository) CountPending(ctx context.Context, aggregateID uuid.UUID) (int64, error) {
	var n int64
	err := r.db.Session(ctx).Raw(`
		SELECT count(*) FROM outbox_events
		 WHERE aggregate_id = ? AND published_at IS NULL`, aggregateID).Scan(&n).Error
	if err != nil {
		return 0, classify(err)
	}
	return n, nil
}

// Claim reivindica eventos pendentes e devidos para esta instância.
//
// Uma instrução só: o SELECT interno escolhe e trava as linhas, e o UPDATE
// externo grava o lease. Duas instruções separadas abririam uma janela entre
// escolher e marcar, e duas instâncias escolheriam a mesma linha.
//
// Os dois mecanismos da cláusula fazem coisas diferentes, e vale não confundir:
// o filtro de lease é o que garante que ninguém publique o que já é de outro;
// o SKIP LOCKED é desempenho, faz quem perde seguir adiante em vez de esperar.
// Verificado por mutação: sem o filtro de lease, dois publicadores enviam TODOS
// os eventos em duplicata; sem o SKIP LOCKED, o resultado continua correto — só
// mais lento, porque o perdedor espera o lock e então não acha nada elegível.
//
// Todos os prazos usam now() do banco. É o único relógio que todas as
// instâncias compartilham; usar o do processo faria uma máquina com relógio
// adiantado segurar registros por mais tempo do que o combinado.
func (r *OutboxRepository) Claim(
	ctx context.Context, dono string, lease time.Duration, limite int,
) ([]outbox.PendingEvent, error) {
	var linhas []outboxRow
	err := r.db.Session(ctx).Raw(`
		UPDATE outbox_events
		   SET locked_by = ?, locked_until = now() + make_interval(secs => ?)
		 WHERE event_id IN (
		       SELECT event_id FROM outbox_events
		        WHERE published_at IS NULL
		          AND next_attempt_at <= now()
		          AND (locked_until IS NULL OR locked_until <= now())
		        ORDER BY next_attempt_at, event_id
		          FOR UPDATE SKIP LOCKED
		        LIMIT ?)
		RETURNING *`,
		dono, lease.Seconds(), limite).Scan(&linhas).Error
	if err != nil {
		return nil, classify(err)
	}

	pendentes := make([]outbox.PendingEvent, 0, len(linhas))
	for _, l := range linhas {
		pendentes = append(pendentes, l.toPending())
	}
	return pendentes, nil
}

// MarkPublished conclui o evento.
//
// A condição é o identificador e published_at ainda vazio, sem mencionar o
// lease: uma publicação que de fato aconteceu precisa ser registrada mesmo que
// o lease tenha expirado no meio dela, senão o evento sairia de novo à toa.
func (r *OutboxRepository) MarkPublished(ctx context.Context, id event.ID) error {
	return r.db.Session(ctx).Exec(`
		UPDATE outbox_events
		   SET published_at = now(), locked_by = NULL, locked_until = NULL, last_error = NULL
		 WHERE event_id = ? AND published_at IS NULL`, uuid.UUID(id)).Error
}

// Reschedule devolve o evento à fila com mais uma tentativa contada.
func (r *OutboxRepository) Reschedule(
	ctx context.Context, id event.ID, espera time.Duration, motivo string,
) error {
	return r.db.Session(ctx).Exec(`
		UPDATE outbox_events
		   SET attempts = attempts + 1,
		       next_attempt_at = now() + make_interval(secs => ?),
		       last_error = ?,
		       locked_by = NULL,
		       locked_until = NULL
		 WHERE event_id = ? AND published_at IS NULL`,
		espera.Seconds(), motivo, uuid.UUID(id)).Error
}

// toPending converte a linha para o que o caso de uso enxerga.
func (l outboxRow) toPending() outbox.PendingEvent {
	p := outbox.PendingEvent{
		ID:            event.ID(l.EventID),
		Type:          event.Type(l.EventType),
		AggregateType: event.AggregateType(l.AggregateType),
		AggregateID:   l.AggregateID.String(),
		Version:       l.Version,
		Payload:       json.RawMessage(l.Payload),
		OccurredAt:    l.OccurredAt,
		Attempts:      l.Attempts,
	}
	if l.CorrelationID != nil {
		p.CorrelationID = *l.CorrelationID
	}
	if l.CausationID != nil {
		p.CausationID = *l.CausationID
	}
	return p
}
