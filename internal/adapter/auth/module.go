package auth

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"go.uber.org/fx"

	"github.com/Lauiskk/munchkin/internal/config"
	"github.com/Lauiskk/munchkin/pkg/logs"
	"github.com/Lauiskk/munchkin/pkg/safe"
)

func newKeySet(cfg config.Config, log *slog.Logger) *KeySet {
	return NewKeySet(KeySetOptions{
		Issuer:             cfg.Auth.Issuer,
		DiscoveryURL:       cfg.Auth.DiscoveryURL,
		RefreshInterval:    cfg.Auth.JWKSRefreshInterval,
		MinRefreshInterval: cfg.Auth.JWKSMinRefreshInterval,
		HTTPTimeout:        cfg.Auth.HTTPTimeout,
		Logger:             log,
	})
}

func newVerifier(keys *KeySet, cfg config.Config) *Verifier {
	return NewVerifier(keys, cfg.Auth.Issuer, cfg.Auth.Audience)
}

// startRefresher amarra o ciclo de vida das chaves ao da aplicação.
//
// A carga inicial acontece no OnStart e a falha dela impede a subida: um
// serviço que aceita tráfego sem conseguir validar token é pior que um serviço
// fora do ar, porque parece saudável enquanto recusa todo mundo.
//
// O encerramento é observável: o worker recebe o cancelamento e o OnStop espera
// por ele antes de devolver, para que a aplicação não feche com uma goroutine
// ainda em pé.
func startRefresher(lc fx.Lifecycle, keys *KeySet, log *slog.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	lc.Append(fx.Hook{
		OnStart: func(startCtx context.Context) error {
			if err := keys.Bootstrap(startCtx); err != nil {
				return err
			}

			wg.Add(1)
			safe.Go(ctx, log, "auth.jwks_refresher", func() {
				defer wg.Done()
				refreshLoop(ctx, keys, log)
			})
			return nil
		},
		OnStop: func(context.Context) error {
			cancel()
			wg.Wait()
			log.Info("auth.jwks_refresher_stopped")
			return nil
		},
	})
}

func refreshLoop(ctx context.Context, keys *KeySet, log *slog.Logger) {
	ticker := time.NewTicker(keys.RefreshInterval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := keys.Refresh(ctx); err != nil && ctx.Err() == nil {
				// Uma atualização periódica que falha não é fatal: as chaves
				// em memória continuam válidas. Mas precisa aparecer, porque
				// falhar sempre significa que a rotação vai nos pegar.
				log.LogAttrs(ctx, slog.LevelError, "auth.jwks_refresh_failed",
					slog.String(logs.KeyError, err.Error()))
			}
		}
	}
}

// Module provê a validação de tokens.
var Module = fx.Module("auth",
	fx.Provide(newKeySet, newVerifier),
	fx.Invoke(startRefresher),
)
