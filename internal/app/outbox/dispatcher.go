package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Lauiskk/munchkin/internal/app"
	"github.com/Lauiskk/munchkin/internal/domain/event"
	"github.com/Lauiskk/munchkin/pkg/backoff"
	"github.com/Lauiskk/munchkin/pkg/logs"
)

const (
	// backoffBase e backoffMax são mais curtos que os das referências
	// pendentes: ali se espera um fato externo chegar, aqui só se espera o
	// transporte voltar. Atraso na outbox é atraso de integração, e o teto de
	// um minuto mantém a retentativa barata sem virar laço.
	backoffBase = time.Second
	backoffMax  = time.Minute

	// maxMotivo limita o que vai para last_error. A mensagem vem do SDK do
	// transporte e não é controlada por nós; guardar ilimitado deixaria uma
	// coluna de texto crescer sem teto, e uma mensagem muito longa poderia
	// arrastar conteúdo que não deveria estar ali.
	maxMotivo = 500
)

// Dispatcher publica os eventos pendentes da outbox.
//
// Roda em TODAS as instâncias, sem eleição de líder: a coordenação é a
// reivindicação por lease no banco.
type Dispatcher struct {
	repo    Repository
	pub     Publisher
	metrics app.Metrics
	log     *slog.Logger
	dono    string
	lote    int
	lease   time.Duration
}

// NewDispatcher monta o publicador.
//
// `dono` identifica esta instância nas linhas que ela reivindica. Precisa ser
// distinto por processo: dois processos com o mesmo identificador não
// conseguiriam distinguir o próprio trabalho do trabalho abandonado do outro.
func NewDispatcher(
	repo Repository, pub Publisher, metrics app.Metrics, log *slog.Logger,
	dono string, lote int, lease time.Duration,
) *Dispatcher {
	return &Dispatcher{
		repo: repo, pub: pub, metrics: metrics, log: log,
		dono: dono, lote: lote, lease: lease,
	}
}

// RunOnce publica uma rodada e devolve quantos eventos saíram.
func (d *Dispatcher) RunOnce(ctx context.Context) (int, error) {
	// A idade é publicada a cada rodada, inclusive quando não há trabalho: um
	// medidor que só é atualizado quando há evento pendente ficaria preso no
	// último valor alto depois que a fila esvaziasse.
	if idade, err := d.repo.OldestPendingAge(ctx); err == nil {
		d.metrics.OutboxPendingAge(idade)
	}

	pendentes, err := d.repo.Claim(ctx, d.dono, d.lease, d.lote)
	if err != nil {
		return 0, fmt.Errorf("reivindicação de eventos: %w", err)
	}

	publicados := 0
	for _, e := range pendentes {
		if ctx.Err() != nil {
			// Encerramento: o que sobrou do lote fica reivindicado e é
			// reassumido quando o lease expirar.
			return publicados, ctx.Err()
		}
		if err := d.publicar(ctx, e); err != nil {
			// Um evento que falha não pode parar a fila: os seguintes podem
			// estar perfeitamente publicáveis, e travar aqui faria um único
			// registro problemático segurar toda a integração.
			d.log.LogAttrs(ctx, slog.LevelWarn, "outbox.publish_failed",
				slog.String(logs.KeyEventID, e.ID.String()),
				slog.String(logs.KeyEventType, string(e.Type)),
				slog.String(logs.KeyAggregateID, e.AggregateID),
				slog.String(logs.KeyCorrelationID, e.CorrelationID),
				slog.Int("attempts", e.Attempts+1),
				slog.String(logs.KeyError, err.Error()))
			continue
		}
		publicados++
	}
	return publicados, nil
}

// publicar monta a mensagem, entrega e confirma.
func (d *Dispatcher) publicar(ctx context.Context, e PendingEvent) error {
	corpo, err := json.Marshal(paraTransporte(e))
	if err != nil {
		return d.adiar(ctx, e, fmt.Errorf("serialização do envelope: %w", err))
	}

	if err := d.pub.Publish(ctx, Message{
		DeduplicationID: e.ID.String(),
		GroupID:         e.AggregateID,
		Body:            corpo,
	}); err != nil {
		return d.adiar(ctx, e, err)
	}

	// A partir daqui o evento JÁ saiu. Se a confirmação falhar, ele será
	// republicado — com o mesmo eventId, que é o que torna a repetição inócua.
	if err := d.repo.MarkPublished(ctx, e.ID); err != nil {
		return fmt.Errorf("confirmação do evento %s: %w", e.ID, err)
	}
	return nil
}

// adiar devolve o evento para a fila e propaga a causa original.
func (d *Dispatcher) adiar(ctx context.Context, e PendingEvent, causa error) error {
	d.metrics.WorkerRetry("outbox.publisher")
	espera := backoff.Exponential(e.Attempts+1, backoffBase, backoffMax)
	if err := d.repo.Reschedule(ctx, e.ID, espera, truncar(causa.Error())); err != nil {
		// Duas falhas de uma vez: a publicação e o registro dela. As duas
		// precisam sobreviver — quem for diagnosticar sem a causa original só
		// veria "não consegui reagendar", sem saber o que deu errado antes.
		// O evento não se perde de qualquer forma: o lease expira e outra
		// instância o retoma.
		return errors.Join(causa, fmt.Errorf("reagendamento: %w", err))
	}
	return causa
}

// paraTransporte monta a forma de transporte a partir da linha persistida.
func paraTransporte(e PendingEvent) event.Wire {
	w := event.Wire{
		EventID:       e.ID,
		EventType:     e.Type,
		AggregateType: e.AggregateType,
		AggregateID:   e.AggregateID,
		CorrelationID: e.CorrelationID,
		OccurredAt:    event.FormatTimestamp(e.OccurredAt),
		Version:       e.Version,
		Data:          e.Payload,
	}
	if e.CausationID != "" {
		w.CausationID = &e.CausationID
	}
	return w
}

// truncar limita o texto guardado como causa da falha.
func truncar(s string) string {
	if len(s) <= maxMotivo {
		return s
	}
	return s[:maxMotivo]
}
