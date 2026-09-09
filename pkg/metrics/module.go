package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/fx"

	"github.com/Lauiskk/munchkin/internal/app"
	"github.com/Lauiskk/munchkin/internal/config"
)

// shutdownTimeout limita a espera pelo encerramento do listener de métricas.
const shutdownTimeout = 5 * time.Second

// serve sobe /metrics num listener PRÓPRIO, em porta separada da API.
//
// Métrica revela volume de operação, número de carteiras e taxa de recusa —
// informação de negócio para quem souber ler. Servir na mesma porta das rotas
// autenticadas a exporia a quem alcança a API; em porta separada, não publicada
// para fora, a superfície de observação fica separada da superfície de negócio.
//
// É um servidor da biblioteca padrão, e não o Fiber da aplicação: nenhum
// middleware de autenticação, correlação ou timeout de negócio faz sentido
// aqui, e reaproveitar o outro traria todos eles junto.
func serve(lc fx.Lifecycle, r *Registry, cfg config.Config, log *slog.Logger) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(r.Gatherer(), promhttp.HandlerOpts{}))

	servidor := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Metrics.Port),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			go func() {
				if err := servidor.ListenAndServe(); err != nil &&
					!errors.Is(err, http.ErrServerClosed) {
					// Observabilidade indisponível não derruba a aplicação: o
					// caminho financeiro não depende dela, e cair por causa do
					// medidor seria trocar um problema pequeno por um grande.
					log.Error("metrics.listener_failed", slog.String("error", err.Error()))
				}
			}()
			log.Info("metrics.listening", slog.Int("port", cfg.Metrics.Port))
			return nil
		},
		OnStop: func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, shutdownTimeout)
			defer cancel()
			return servidor.Shutdown(ctx)
		},
	})
}

// Module provê o registro de métricas e o listener que o expõe.
var Module = fx.Module("metrics",
	fx.Provide(
		New,
		func(r *Registry) app.Metrics { return r },
	),
	fx.Invoke(serve),
)
