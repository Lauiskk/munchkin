//go:build integration

// Package concurrency_test verifica as garantias sob concorrência real.
//
// Não há dublê aqui: PostgreSQL de verdade, goroutines de verdade, e o mesmo
// caminho de código que a aplicação usa. Uma corrida só aparece quando há
// corrida, e um dublê determinístico esconde exatamente o que se quer ver.
package concurrency_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/Lauiskk/munchkin/internal/adapter/postgres"
	appwagering "github.com/Lauiskk/munchkin/internal/app/wagering"
	appwallet "github.com/Lauiskk/munchkin/internal/app/wallet"
	"github.com/Lauiskk/munchkin/internal/config"
	"github.com/Lauiskk/munchkin/internal/domain/money"
	domainwallet "github.com/Lauiskk/munchkin/internal/domain/wallet"
)

const (
	testDBName        = "munchkin"
	testOwnerUser     = "munchkin_owner"
	testOwnerPassword = "teste-owner"
	testAppUser       = "munchkin_app"
	testAppPassword   = "teste-app"
	postgresImage     = "postgres:18.6-alpine"
)

type relogio struct{}

func (relogio) Now() time.Time { return time.Now().UTC() }

// cenario reúne o que os testes de concorrência precisam.
type cenario struct {
	db        *postgres.Database
	opener    *appwallet.Opener
	processor *appwagering.Processor
	ctx       context.Context
}

// novoCenario sobe um PostgreSQL migrado e monta a aplicação sobre ele.
//
// O pool é dimensionado acima do paralelismo dos testes: com pool menor que o
// número de goroutines, a espera por conexão mascararia a contenção que se quer
// medir — o teste passaria por serialização acidental, não pelo lock da carteira.
func novoCenario(t *testing.T) cenario {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	script, err := filepath.Abs(filepath.Join("..", "..", "deploy", "postgres", "10-roles.sh"))
	require.NoError(t, err)

	container, err := tcpostgres.Run(ctx, postgresImage,
		tcpostgres.WithDatabase(testDBName),
		tcpostgres.WithUsername(testOwnerUser),
		tcpostgres.WithPassword(testOwnerPassword),
		tcpostgres.WithInitScripts(script),
		testcontainers.WithEnv(map[string]string{
			"MUNCHKIN_APP_USER":     testAppUser,
			"MUNCHKIN_APP_PASSWORD": testAppPassword,
		}),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(2*time.Minute)),
	)
	require.NoError(t, err, "Docker precisa estar em execução")
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = container.Terminate(c)
	})

	host, err := container.Host(ctx)
	require.NoError(t, err)
	porta, err := container.MappedPort(ctx, "5432/tcp")
	require.NoError(t, err)

	dbCfg := config.DB{
		Host: host, Port: int(porta.Num()), Name: testDBName,
		User: testAppUser, Password: config.Secret(testAppPassword),
		SSLMode: "disable", MaxOpenConns: 60, MaxIdleConns: 20,
		ConnMaxLifetime: 30 * time.Minute, ConnMaxIdleTime: 5 * time.Minute,
		ConnectTimeout: 10 * time.Second,
	}

	log := slog.New(slog.NewJSONHandler(io.Discard, nil))

	ownerCfg := dbCfg
	ownerCfg.User = testOwnerUser
	ownerCfg.Password = config.Secret(testOwnerPassword)
	migrator, err := postgres.NewMigrator(ownerCfg, log)
	require.NoError(t, err)
	require.NoError(t, migrator.Up())
	require.NoError(t, migrator.Close())

	db, err := postgres.Open(config.Config{DB: dbCfg, Log: config.Log{Level: "info"}}, log)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	wallets := postgres.NewWalletRepository(db)
	transacoes := postgres.NewTransactionRepository(db)
	entradas := postgres.NewLedgerRepository(db)
	outbox := postgres.NewOutboxRepository(db)

	return cenario{
		db:        db,
		opener:    appwallet.NewOpener(db, wallets, transacoes, entradas, outbox, relogio{}),
		processor: appwagering.NewProcessor(db, wallets, transacoes, entradas, outbox, relogio{}),
		ctx:       context.Background(),
	}
}

func (c cenario) abrirCarteira(t *testing.T, saldo string) (domainwallet.ID, domainwallet.PlayerID) {
	t.Helper()
	jogador, err := domainwallet.NewPlayerID()
	require.NoError(t, err)
	valor, err := money.Parse(saldo, money.BRL)
	require.NoError(t, err)

	out, err := c.opener.Open(c.ctx, appwallet.OpenInput{PlayerID: jogador, InitialBalance: valor})
	require.NoError(t, err)
	return out.Wallet.ID(), jogador
}

func (c cenario) escalar(t *testing.T, query string, args ...any) int64 {
	t.Helper()
	var n int64
	require.NoError(t, c.db.Session(c.ctx).Raw(query, args...).Scan(&n).Error)
	return n
}
