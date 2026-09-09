// Package ops serve a superfície de operação: métricas e documentação.
//
// Separada da superfície de negócio de propósito, e não por organização. Métrica
// revela volume de operação e taxa de recusa; o contrato revela o mapa da API.
// Nenhum dos dois é dado de cliente, mas nenhum dos dois precisa estar
// acessível a quem alcança a API — e num ambiente real esta porta não sai da
// rede interna.
//
// É um servidor da biblioteca padrão, e não o Fiber da aplicação: nenhum
// middleware de autenticação, correlação ou timeout de negócio faz sentido aqui,
// e reaproveitar o outro traria todos eles junto.
package ops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/fx"

	"github.com/Lauiskk/munchkin/api"
	"github.com/Lauiskk/munchkin/internal/config"
	"github.com/Lauiskk/munchkin/pkg/metrics"
)

// shutdownTimeout limita a espera pelo encerramento do listener.
const shutdownTimeout = 5 * time.Second

// paginaDocs é a casca da UI de documentação.
//
// O documento vem do binário; só os arquivos da interface vêm de CDN, e com
// versão fixada. Quem abre a página tem navegador com internet — o container
// não precisa de saída para a rede, e continua sem ela.
const paginaDocs = `<!DOCTYPE html>
<html lang="pt-BR">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Munchkin — contrato da API</title>
  <link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.29.4/swagger-ui.css">
</head>
<body>
  <div id="swagger"></div>
  <script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.29.4/swagger-ui-bundle.js"></script>
  <script>
    window.ui = SwaggerUIBundle({
      url: "/openapi.yaml",
      dom_id: "#swagger",
      deepLinking: true,
      docExpansion: "list",
      defaultModelsExpandDepth: 1
    });
  </script>
</body>
</html>`

// handler monta as rotas da superfície de operação.
func handler(r *metrics.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(r.Gatherer(), promhttp.HandlerOpts{}))

	mux.HandleFunc("/openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		_, _ = w.Write(api.OpenAPI)
	})

	mux.HandleFunc("/docs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(paginaDocs))
	})

	return mux
}

// serve amarra o listener ao ciclo de vida.
func serve(lc fx.Lifecycle, r *metrics.Registry, cfg config.Config, log *slog.Logger) {
	servidor := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Metrics.Port),
		Handler:           handler(r),
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
					log.Error("ops.listener_failed", slog.String("error", err.Error()))
				}
			}()
			log.Info("ops.listening",
				slog.Int("port", cfg.Metrics.Port),
				slog.String("paths", "/metrics /docs /openapi.yaml"))
			return nil
		},
		OnStop: func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, shutdownTimeout)
			defer cancel()
			return servidor.Shutdown(ctx)
		},
	})
}

// Module provê a superfície de operação.
var Module = fx.Module("ops",
	fx.Invoke(serve),
)
