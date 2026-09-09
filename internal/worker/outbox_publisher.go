package worker

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/google/uuid"
	"go.uber.org/fx"

	"github.com/Lauiskk/munchkin/internal/app/outbox"
	"github.com/Lauiskk/munchkin/internal/config"
)

// newOutboxDispatcher monta o publicador com a identidade desta instância.
//
// O identificador combina o nome da máquina com um valor único do processo. O
// nome sozinho não serve: dois processos no mesmo host, ou o mesmo host depois
// de um reinício, dividiriam a identidade e um assumiria como seu o trabalho
// abandonado pelo outro — que é exatamente o que o lease existe para evitar.
func newOutboxDispatcher(
	repo outbox.Repository, pub outbox.Publisher, cfg config.Config, log *slog.Logger,
) (*outbox.Dispatcher, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("identidade do publicador: %w", err)
	}

	maquina, err := os.Hostname()
	if err != nil {
		// Não é motivo para recusar a subida: o identificador continua único
		// pelo UUID, e o nome da máquina é conveniência para quem opera.
		maquina = "desconhecida"
	}

	return outbox.NewDispatcher(repo, pub, log,
		fmt.Sprintf("%s/%s", maquina, id),
		cfg.Worker.OutboxBatch, cfg.Worker.OutboxLease), nil
}

// startOutboxPublisher amarra o publicador da outbox ao ciclo de vida.
func startOutboxPublisher(
	lc fx.Lifecycle, dispatcher *outbox.Dispatcher, cfg config.Config, log *slog.Logger,
) {
	iniciar(lc, "outbox.publisher", dispatcher.RunOnce, cfg.Worker.OutboxInterval, log)
}
