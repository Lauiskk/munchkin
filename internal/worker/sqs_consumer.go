package worker

import (
	"log/slog"

	"go.uber.org/fx"

	"github.com/Lauiskk/munchkin/internal/adapter/sqs"
	"github.com/Lauiskk/munchkin/internal/adapter/tracing"
	"github.com/Lauiskk/munchkin/internal/config"
)

// startTransactionConsumer amarra o consumidor de operações ao ciclo de vida.
//
// O intervalo do ticker é curto porque quem segura a espera é o long polling
// dentro da própria busca: uma fila vazia devolve depois de vinte segundos, e
// não vinte vezes por segundo.
func startTransactionConsumer(
	lc fx.Lifecycle, consumer *sqs.Consumer, cfg config.Config,
	log *slog.Logger, tracer *tracing.Tracer,
) {
	iniciar(lc, "sqs.consumer", consumer.RunOnce, cfg.Worker.ConsumerInterval, log, tracer)
}
