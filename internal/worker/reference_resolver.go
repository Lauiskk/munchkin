// Package worker reúne os processos de fundo da aplicação.
//
// Todos rodam em TODAS as instâncias, sem eleição de líder. A coordenação é do
// banco, por SKIP LOCKED — o enunciado trata como eliminatória a dependência de
// uma única instância para funcionar corretamente.
package worker

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/fx"

	"github.com/Lauiskk/munchkin/internal/adapter/tracing"

	appwagering "github.com/Lauiskk/munchkin/internal/app/wagering"
	"github.com/Lauiskk/munchkin/internal/config"
	"github.com/Lauiskk/munchkin/pkg/logs"
	"github.com/Lauiskk/munchkin/pkg/safe"
)

// startReferenceResolver amarra o resolvedor de referências ao ciclo de vida.
func startReferenceResolver(
	lc fx.Lifecycle, resolver *appwagering.Resolver, cfg config.Config,
	log *slog.Logger, tracer *tracing.Tracer,
) {
	iniciar(lc, "reference.resolver", resolver.RunOnce, cfg.Worker.ReferenceInterval, log, tracer)
}

// varredura é uma rodada de trabalho de fundo: devolve quantos itens tratou.
type varredura func(ctx context.Context) (int, error)

func laco(ctx context.Context, nome string, rodar varredura, intervalo time.Duration, log *slog.Logger, tracer trace.Tracer) {
	ticker := time.NewTicker(intervalo)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rodada(ctx, nome, rodar, log, tracer)
		}
	}
}

// rodada executa uma varredura, protegida contra pânico.
//
// O recover é por ITERAÇÃO: um pânico ao tratar um item específico não pode
// encerrar o worker, senão uma única linha problemática tiraria o processo de
// fundo do ar até o próximo reinício.
func rodada(ctx context.Context, nome string, rodar varredura, log *slog.Logger, tracer trace.Tracer) {
	defer safe.Recover(ctx, log, nome)

	// Um span por RODADA. Sem ele, as consultas que o worker faz aparecem como
	// spans soltos, sem pai e sem contexto — e quem abre o Jaeger vê dezenas de
	// `gorm.Row` órfãos sem saber de quem são.
	//
	// Com o tracing desligado o tracer é nulo, e isto não aloca nada.
	ctx, span := tracer.Start(ctx, "worker "+nome, trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()

	tratados, err := rodar(ctx)
	span.SetAttributes(attribute.Int("worker.processed", tratados))
	if err != nil && ctx.Err() == nil {
		span.RecordError(err)
		log.LogAttrs(ctx, slog.LevelError, "worker.round_failed",
			slog.String("worker", nome),
			slog.String(logs.KeyError, err.Error()))
		return
	}
	if tratados > 0 {
		log.LogAttrs(ctx, slog.LevelInfo, "worker.round",
			slog.String("worker", nome),
			slog.Int("processed", tratados))
	}
}

// iniciar é o esqueleto comum: sobe a goroutine no OnStart e espera por ela no
// OnStop. Sem a espera, a aplicação fecharia com uma goroutine ainda em pé e o
// pool do banco poderia ser fechado sob ela.
func iniciar(
	lc fx.Lifecycle, nome string, rodar varredura, intervalo time.Duration,
	log *slog.Logger, tracer *tracing.Tracer,
) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			wg.Add(1)
			// safe.Go porque um pânico em goroutine não é alcançado pelo
			// recover de quem a iniciou: derrubaria o processo inteiro.
			safe.Go(ctx, log, nome, func() {
				defer wg.Done()
				laco(ctx, nome, rodar, intervalo, log, tracer)
			})
			log.Info("worker.started",
				slog.String("worker", nome),
				slog.Duration("interval", intervalo))
			return nil
		},
		OnStop: func(context.Context) error {
			cancel()
			wg.Wait()
			log.Info("worker.stopped", slog.String("worker", nome))
			return nil
		},
	})
}

// Module provê os processos de fundo.
var Module = fx.Module("worker",
	fx.Provide(newOutboxDispatcher),
	fx.Invoke(
		startReferenceResolver,
		startOutboxPublisher,
		startTransactionConsumer,
	),
)
