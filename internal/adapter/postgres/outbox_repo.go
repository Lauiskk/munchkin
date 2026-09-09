package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

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
