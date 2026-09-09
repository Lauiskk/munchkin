//go:build integration

package integration_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/internal/adapter/postgres"
	"github.com/Lauiskk/munchkin/internal/config"
)

func abrir(t *testing.T, dbCfg config.DB) *postgres.Database {
	t.Helper()
	db, err := postgres.Open(
		config.Config{DB: dbCfg, Log: config.Log{Level: "info"}},
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// Este é o teste que fecha o risco de a verificação inteira depender de dublê:
// PostgreSQL de verdade, subindo e descendo dentro do teste, sem ambiente
// pré-montado.
func TestConexaoComPostgresReal(t *testing.T) {
	db := abrir(t, startPostgres(t))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	require.NoError(t, db.Ping(ctx))

	var versao string
	require.NoError(t, db.Session(ctx).Raw("SELECT version()").Scan(&versao).Error)
	assert.Contains(t, versao, "PostgreSQL 18")

	stats := db.Stats()
	assert.LessOrEqual(t, stats.OpenConnections, 10, "o pool respeita o limite configurado")
}

// A separação de papéis é o que torna possível impor imutabilidade no banco.
// Se a aplicação conectasse como dono, nenhum REVOKE a alcançaria.
func TestAplicacaoNaoEhDonaDoSchema(t *testing.T) {
	cfg := startPostgres(t)
	app := abrir(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := app.Session(ctx).Exec("CREATE TABLE nao_deveria_existir (id int)").Error
	require.Error(t, err, "a aplicação não pode criar estrutura")
	assert.Contains(t, err.Error(), "permission denied")

	var superusuario bool
	require.NoError(t, app.Session(ctx).
		Raw("SELECT rolsuper FROM pg_roles WHERE rolname = current_user").
		Scan(&superusuario).Error)
	assert.False(t, superusuario, "superusuário ignora toda constraint de privilégio")
}

// O REVOKE que tornará o ledger append-only precisa funcionar contra o papel da
// aplicação. Este teste prova o mecanismo antes de a migration depender dele.
func TestRevogacaoDeUpdateEDeleteAlcancaAAplicacao(t *testing.T) {
	cfg := startPostgres(t)
	dono := abrir(t, asOwner(cfg))
	app := abrir(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	require.NoError(t, dono.Session(ctx).Exec(`
		CREATE TABLE apenas_insercao (id serial PRIMARY KEY, valor int NOT NULL);
		REVOKE UPDATE, DELETE ON apenas_insercao FROM munchkin_app;
	`).Error)

	require.NoError(t, app.Session(ctx).Exec("INSERT INTO apenas_insercao (valor) VALUES (1)").Error,
		"inserir continua permitido: o ledger é append-only, não somente leitura")

	err := app.Session(ctx).Exec("UPDATE apenas_insercao SET valor = 2").Error
	require.Error(t, err)
	assert.Contains(t, err.Error(), "permission denied")

	err = app.Session(ctx).Exec("DELETE FROM apenas_insercao").Error
	require.Error(t, err)
	assert.Contains(t, err.Error(), "permission denied")
}

func TestTransacaoConfirmaAoFinal(t *testing.T) {
	cfg := startPostgres(t)
	dono := abrir(t, asOwner(cfg))
	app := abrir(t, cfg)
	ctx := criarTabelaDeTeste(t, dono)

	require.NoError(t, app.Within(ctx, func(ctx context.Context) error {
		return app.Session(ctx).Exec("INSERT INTO caixa (valor) VALUES (10)").Error
	}))

	assert.Equal(t, int64(1), contar(t, app, ctx))
}

// Erro devolvido pelo bloco desfaz tudo. É a atomicidade que o enunciado exige:
// estado, saldo, ledger e eventos são confirmados juntos ou não são.
func TestTransacaoDesfazEmCasoDeErro(t *testing.T) {
	cfg := startPostgres(t)
	dono := abrir(t, asOwner(cfg))
	app := abrir(t, cfg)
	ctx := criarTabelaDeTeste(t, dono)

	falha := errors.New("regra de negócio recusou")
	err := app.Within(ctx, func(ctx context.Context) error {
		if err := app.Session(ctx).Exec("INSERT INTO caixa (valor) VALUES (10)").Error; err != nil {
			return err
		}
		return falha
	})

	require.ErrorIs(t, err, falha)
	assert.Equal(t, int64(0), contar(t, app, ctx), "nada pode ter sobrado")
}

// Um pânico precisa desfazer a transação antes de subir. Sem isso a conexão
// volta ao pool com transação aberta, e a próxima operação a pegá-la herda o
// estado sujo.
func TestPanicoDesfazATransacaoERepropaga(t *testing.T) {
	cfg := startPostgres(t)
	dono := abrir(t, asOwner(cfg))
	app := abrir(t, cfg)
	ctx := criarTabelaDeTeste(t, dono)

	assert.PanicsWithValue(t, "estouro no meio da transação", func() {
		_ = app.Within(ctx, func(ctx context.Context) error {
			_ = app.Session(ctx).Exec("INSERT INTO caixa (valor) VALUES (99)").Error
			panic("estouro no meio da transação")
		})
	})

	assert.Equal(t, int64(0), contar(t, app, ctx), "o pânico não pode deixar escrita confirmada")

	// A conexão precisa continuar utilizável depois do pânico.
	require.NoError(t, app.Within(ctx, func(ctx context.Context) error {
		return app.Session(ctx).Exec("INSERT INTO caixa (valor) VALUES (1)").Error
	}))
	assert.Equal(t, int64(1), contar(t, app, ctx))
}

// Chamada aninhada não pode abrir uma segunda transação: o erro do bloco
// interno tem que desfazer o externo também, senão a atomicidade quebra em
// silêncio, que é o pior jeito de quebrá-la.
func TestTransacaoAninhadaCompartilhaAMesma(t *testing.T) {
	cfg := startPostgres(t)
	dono := abrir(t, asOwner(cfg))
	app := abrir(t, cfg)
	ctx := criarTabelaDeTeste(t, dono)

	falha := errors.New("falha no bloco interno")
	err := app.Within(ctx, func(ctx context.Context) error {
		require.True(t, postgres.InTransaction(ctx))
		if err := app.Session(ctx).Exec("INSERT INTO caixa (valor) VALUES (1)").Error; err != nil {
			return err
		}
		return app.Within(ctx, func(ctx context.Context) error {
			if err := app.Session(ctx).Exec("INSERT INTO caixa (valor) VALUES (2)").Error; err != nil {
				return err
			}
			return falha
		})
	})

	require.ErrorIs(t, err, falha)
	assert.Equal(t, int64(0), contar(t, app, ctx),
		"a escrita do bloco externo também tem que ser desfeita")
}

// Fora de uma transação, Session usa o pool — e a escrita é confirmada sozinha.
// O teste registra a diferença para que ela seja escolha, e não descoberta.
func TestForaDeTransacaoAEscritaEhImediata(t *testing.T) {
	cfg := startPostgres(t)
	dono := abrir(t, asOwner(cfg))
	app := abrir(t, cfg)
	ctx := criarTabelaDeTeste(t, dono)

	assert.False(t, postgres.InTransaction(ctx))
	require.NoError(t, app.Session(ctx).Exec("INSERT INTO caixa (valor) VALUES (7)").Error)
	assert.Equal(t, int64(1), contar(t, app, ctx))
}

func criarTabelaDeTeste(t *testing.T, dono *postgres.Database) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	require.NoError(t, dono.Session(ctx).Exec(
		"CREATE TABLE caixa (id serial PRIMARY KEY, valor int NOT NULL)").Error)
	return ctx
}

func contar(t *testing.T, db *postgres.Database, ctx context.Context) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.Session(ctx).Raw("SELECT count(*) FROM caixa").Scan(&n).Error)
	return n
}
