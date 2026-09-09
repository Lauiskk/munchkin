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

	"go.uber.org/fx"

	appwagering "github.com/Lauiskk/munchkin/internal/app/wagering"
	"github.com/Lauiskk/munchkin/internal/config"
	"github.com/Lauiskk/munchkin/pkg/logs"
	"github.com/Lauiskk/munchkin/pkg/safe"
)

// startReferenceResolver amarra o resolvedor de referências ao ciclo de vida.
//
// O encerramento é observável: o worker recebe o cancelamento e o OnStop espera
// por ele antes de devolver. Sem a espera, a aplicação fecharia com uma
// goroutine ainda em pé e o pool do banco poderia ser fechado sob ela.
func startReferenceResolver(
	lc fx.Lifecycle, resolver *appwagering.Resolver, cfg config.Config, log *slog.Logger,
) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			wg.Add(1)
			// safe.Go porque um pânico em goroutine não é alcançado pelo
			// recover de quem a iniciou: derrubaria o processo inteiro.
			safe.Go(ctx, log, "reference.resolver", func() {
				defer wg.Done()
				laco(ctx, resolver, cfg.Worker.ReferenceInterval, log)
			})
			log.Info("worker.started",
				slog.String("worker", "reference.resolver"),
				slog.Duration("interval", cfg.Worker.ReferenceInterval))
			return nil
		},
		OnStop: func(context.Context) error {
			cancel()
			wg.Wait()
			log.Info("worker.stopped", slog.String("worker", "reference.resolver"))
			return nil
		},
	})
}

func laco(ctx context.Context, resolver *appwagering.Resolver, intervalo time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(intervalo)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rodada(ctx, resolver, log)
		}
	}
}

// rodada executa uma varredura, protegida contra pânico.
//
// O recover é por ITERAÇÃO: um pânico ao tratar uma pendência específica não
// pode encerrar o worker, senão uma única linha problemática tiraria a retomada
// do ar até o próximo reinício.
func rodada(ctx context.Context, resolver *appwagering.Resolver, log *slog.Logger) {
	defer safe.Recover(ctx, log, "reference.resolver")

	tratadas, err := resolver.RunOnce(ctx)
	if err != nil && ctx.Err() == nil {
		log.LogAttrs(ctx, slog.LevelError, "resolver.round_failed",
			slog.String(logs.KeyError, err.Error()))
		return
	}
	if tratadas > 0 {
		log.LogAttrs(ctx, slog.LevelInfo, "resolver.round",
			slog.Int("resolved", tratadas))
	}
}

// Module provê os processos de fundo.
var Module = fx.Module("worker",
	fx.Invoke(startReferenceResolver),
)
