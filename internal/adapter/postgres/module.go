package postgres

import (
	"context"
	"log/slog"
	"time"

	"go.uber.org/fx"

	"github.com/Lauiskk/munchkin/internal/adapter/http/handler"
	appwagering "github.com/Lauiskk/munchkin/internal/app/wagering"
	appwallet "github.com/Lauiskk/munchkin/internal/app/wallet"
	"github.com/Lauiskk/munchkin/internal/config"
	"github.com/Lauiskk/munchkin/pkg/logs"
)

// pingTimeout limita a sonda de prontidão. Um banco que demora mais que isto
// para responder a um ping não está utilizável para o caminho financeiro.
const pingTimeout = 2 * time.Second

// readinessCheck responde pela prontidão do PostgreSQL.
type readinessCheck struct{ db *Database }

func (c readinessCheck) Name() string { return "postgres" }

func (c readinessCheck) Check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	return c.db.Ping(ctx)
}

// newReadinessCheck registra a verificação no grupo lido pelo health check.
// O handler não conhece o PostgreSQL: ele conhece a interface, e cada
// adaptador se inscreve.
func newReadinessCheck(db *Database) handler.ReadinessCheck {
	return readinessCheck{db: db}
}

// register amarra a conexão ao ciclo de vida da aplicação.
func register(lc fx.Lifecycle, db *Database, cfg config.Config, log *slog.Logger) {
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			// A conexão é verificada na subida. Um banco inalcançável precisa
			// impedir a aplicação de subir: aceitar tráfego para falhar em
			// toda requisição é pior que não subir, porque o orquestrador
			// considera o processo saudável e não o substitui.
			ctx, cancel := context.WithTimeout(ctx, cfg.DB.ConnectTimeout)
			defer cancel()

			if err := db.Ping(ctx); err != nil {
				return err
			}
			log.Info("db.connected",
				slog.String("host", cfg.DB.Host),
				slog.Int("port", cfg.DB.Port),
				slog.String("database", cfg.DB.Name),
				slog.String("user", cfg.DB.User),
				slog.Int("max_open_conns", cfg.DB.MaxOpenConns))
			return nil
		},
		OnStop: func(context.Context) error {
			// O Fx encerra na ordem inversa da inicialização, então o pool
			// fecha depois dos componentes que o usam. É o que evita derrubar
			// a conexão sob um worker ainda em execução.
			if err := db.Close(); err != nil {
				log.Error("db.close_failed", slog.String(logs.KeyError, err.Error()))
				return err
			}
			log.Info("db.closed")
			return nil
		},
	})
}

// Module provê o acesso ao PostgreSQL.
var Module = fx.Module("postgres",
	fx.Provide(
		Open,
		fx.Annotate(newReadinessCheck, fx.ResultTags(`group:"readiness"`)),

		// Repositórios concretos ligados às portas que os casos de uso
		// declararam. É aqui, e só aqui, que a implementação encontra a
		// interface: o caso de uso nunca vê o tipo concreto.
		NewWalletRepository,
		NewTransactionRepository,
		NewLedgerRepository,
		NewOutboxRepository,
		func(r *WalletRepository) appwallet.Repository { return r },
		func(r *TransactionRepository) appwallet.TransactionRepository { return r },
		func(r *LedgerRepository) appwallet.LedgerRepository { return r },
		func(r *OutboxRepository) appwallet.OutboxRepository { return r },
		func(db *Database) appwallet.TxManager { return db },

		func(db *Database) appwagering.TxManager { return db },
		func(r *WalletRepository) appwagering.WalletRepository { return r },
		func(r *TransactionRepository) appwagering.TransactionRepository { return r },
		func(r *LedgerRepository) appwagering.LedgerRepository { return r },
		func(r *OutboxRepository) appwagering.OutboxRepository { return r },
		func(r *TransactionRepository) appwagering.PendingRepository { return r },
	),
	fx.Invoke(register),
)
