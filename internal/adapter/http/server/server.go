// Package server monta o servidor HTTP e amarra seu ciclo de vida ao Fx.
package server

import (
	"context"
	"log/slog"
	"net"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"go.uber.org/fx"

	"github.com/Lauiskk/munchkin/internal/adapter/auth"
	"github.com/Lauiskk/munchkin/internal/adapter/http/handler"
	"github.com/Lauiskk/munchkin/internal/adapter/http/middleware"
	"github.com/Lauiskk/munchkin/internal/adapter/http/router"
	"github.com/Lauiskk/munchkin/internal/config"
	"github.com/Lauiskk/munchkin/pkg/logs"
	"github.com/Lauiskk/munchkin/pkg/safe"
)

// readinessTimeout limita o conjunto das verificações de prontidão.
const readinessTimeout = 3 * time.Second

// New monta o aplicativo Fiber com a cadeia de middleware e as rotas.
func New(
	cfg config.Config,
	log *slog.Logger,
	health *handler.Health,
	wallets *handler.Wallet,
	verifier middleware.TokenVerifier,
	isPublic router.PublicPaths,
) *fiber.App {
	app := fiber.New(fiber.Config{
		AppName:               "munchkin",
		ErrorHandler:          middleware.ErrorHandler(log),
		ReadTimeout:           cfg.HTTP.ReadTimeout,
		WriteTimeout:          cfg.HTTP.WriteTimeout,
		DisableStartupMessage: true,
	})

	// A ordem importa. A correlação vem primeiro para que tudo que acontecer
	// depois — inclusive um pânico — já tenha identificador. O recover vem em
	// seguida, envolvendo handler e demais middlewares. O contexto da aplicação
	// é montado antes do log, que já o usa para carregar a correlação.
	app.Use(middleware.Correlation())
	app.Use(recover.New(recover.Config{
		EnableStackTrace: true,
		StackTraceHandler: func(c *fiber.Ctx, e any) {
			log.LogAttrs(c.UserContext(), slog.LevelError, "http.panic",
				slog.Any("panic", e),
				slog.String("method", c.Method()),
				slog.String("path", c.Path()))
		},
	}))
	app.Use(middleware.RequestContext(cfg.HTTP.RequestTimeout))
	app.Use(middleware.Logging(log))

	// A autenticação roda antes do roteamento, então uma rota inexistente
	// responde 401 a quem não se identificou, e não 404. É deliberado: 404
	// contaria a um chamador anônimo quais caminhos existem, e num serviço
	// financeiro o mapa da API não é informação pública.
	app.Use(middleware.Authenticate(verifier, isPublic, log))

	router.Register(app, health, wallets)
	return app
}

// Run amarra o servidor ao ciclo de vida do Fx.
func Run(lc fx.Lifecycle, sd fx.Shutdowner, app *fiber.App, cfg config.Config, log *slog.Logger) {
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			// O socket é aberto aqui, e não dentro da goroutine, de propósito:
			// uma porta ocupada tem que derrubar a subida com erro claro. Se o
			// Listen acontecesse na goroutine, a aplicação subiria "com
			// sucesso" e só um log perdido diria que ela não escuta nada.
			var lc net.ListenConfig
			ln, err := lc.Listen(ctx, "tcp", cfg.HTTP.Addr())
			if err != nil {
				return err
			}

			log.Info("http.listening", slog.String("addr", cfg.HTTP.Addr()),
				slog.String("env", cfg.App.Env))

			// safe.Go porque um pânico em goroutine não é alcançado pelo
			// recover de quem a iniciou: derrubaria o processo inteiro.
			safe.Go(ctx, log, "http.listener", func() {
				if err := app.Listener(ln); err != nil {
					log.Error("http.serve_failed", slog.String(logs.KeyError, err.Error()))
					// O servidor morreu depois de subir. Encerrar a aplicação
					// inteira é o certo: um processo vivo que não atende é pior
					// que um processo morto, porque o orquestrador não o troca.
					if err := sd.Shutdown(); err != nil {
						log.Error("http.shutdown_signal_failed",
							slog.String(logs.KeyError, err.Error()))
					}
				}
			})
			return nil
		},
		OnStop: func(ctx context.Context) error {
			log.Info("http.shutting_down")

			ctx, cancel := context.WithTimeout(ctx, cfg.HTTP.ShutdownTimeout)
			defer cancel()

			// Para de aceitar conexões novas e aguarda as em andamento
			// concluírem dentro do prazo.
			if err := app.ShutdownWithContext(ctx); err != nil {
				return err
			}
			log.Info("http.stopped")
			return nil
		},
	})
}

// healthParams coleta as verificações de prontidão que os adaptadores
// registrarem no grupo, sem que o servidor precise conhecer cada uma.
type healthParams struct {
	fx.In
	Log    *slog.Logger
	Checks []handler.ReadinessCheck `group:"readiness"`
}

func newHealth(p healthParams) *handler.Health {
	return handler.NewHealth(p.Log, readinessTimeout, p.Checks...)
}

// Module provê o servidor HTTP.
var Module = fx.Module("http",
	fx.Provide(
		newHealth,
		handler.NewWallet,
		router.NewPublicPaths,
		// O verificador concreto é ligado à interface que o middleware pede.
		// Sem esta ponte, o grafo entregaria o tipo concreto e a cadeia
		// deixaria de ser exercitável com um dublê nos testes.
		func(v *auth.Verifier) middleware.TokenVerifier { return v },
		New,
	),
	fx.Invoke(Run),
)
