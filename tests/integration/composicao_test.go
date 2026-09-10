//go:build integration

package integration_test

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"

	"github.com/Lauiskk/munchkin/internal/adapter/postgres"
	"github.com/Lauiskk/munchkin/internal/composition"
	"github.com/Lauiskk/munchkin/internal/config"
)

// diarioSincronizado acumula o log da aplicação para que o teste possa cobrá-lo.
type diarioSincronizado struct {
	mu    sync.Mutex
	texto strings.Builder
}

func (d *diarioSincronizado) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.texto.Write(p)
}

func (d *diarioSincronizado) String() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.texto.String()
}

// configuracaoCompleta monta a configuração apontando para os containers da
// suíte.
//
// Porta zero nos dois listeners: o sistema operacional escolhe uma livre. Fixar
// porta faria o teste falhar quando o ambiente local já estivesse no ar, que é
// justamente quando alguém roda a suíte.
func configuracaoCompleta(t *testing.T) config.Config {
	t.Helper()

	dbCfg := migrado(t)
	awsCfg := filasDeOperacao(t)
	base, err := keycloakCompartilhado()
	require.NoError(t, err)

	return config.Config{
		App:  config.App{Env: "desenvolvimento"},
		HTTP: config.HTTP{Port: 0, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, RequestTimeout: 5 * time.Second, ShutdownTimeout: 5 * time.Second},
		Log:  config.Log{Level: "info", Format: "json"},
		Auth: config.Auth{
			Issuer:                 base + "/realms/" + realm,
			DiscoveryURL:           base + "/realms/" + realm + "/.well-known/openid-configuration",
			Audience:               audience,
			JWKSRefreshInterval:    time.Minute,
			JWKSMinRefreshInterval: time.Second,
			HTTPTimeout:            5 * time.Second,
		},
		DB: dbCfg,
		Worker: config.Worker{
			ReferenceInterval: time.Second, OutboxInterval: time.Second,
			OutboxBatch: 10, OutboxLease: 30 * time.Second,
			ConsumerInterval: time.Second, ConsumerBatch: 10, ConsumerWait: 0,
		},
		AWS:     awsCfg,
		Metrics: config.Metrics{Port: 0},
	}
}

// AC-4, AC-5, AC-6 — a composição inteira sobe e encerra liberando recursos.
//
// O §13 pede esta verificação por nome, e a razão é boa: um grafo de injeção
// monta tudo por reflexão, então uma dependência faltando ou um ciclo só
// aparecem em tempo de execução. Compilar não prova que a aplicação sobe.
func TestComposicaoFxSobeEEncerraLiberandoRecursos(t *testing.T) {
	cfg := configuracaoCompleta(t)

	var diario diarioSincronizado
	log := slog.New(slog.NewJSONHandler(&diario, nil))

	var db *postgres.Database
	app := fx.New(
		fx.StartTimeout(2*time.Minute),
		fx.StopTimeout(time.Minute),

		fx.Supply(cfg),
		fx.Supply(log),
		fx.WithLogger(func(l *slog.Logger) fxevent.Logger {
			return &fxevent.SlogLogger{Logger: l}
		}),

		// O MESMO grafo que o cmd/api monta — literalmente a mesma variável.
		// Uma lista própria de teste verificaria uma composição que não é a que
		// roda, e foi assim que ela ficou desatualizada quando o tracing entrou.
		composition.Aplicacao,

		fx.Populate(&db),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	require.NoError(t, app.Start(ctx), "a aplicação inteira precisa subir")
	require.NoError(t, db.Ping(ctx), "o banco fica utilizável enquanto a aplicação está de pé")

	subida := diario.String()
	for _, nome := range []string{"reference.resolver", "outbox.publisher", "sqs.consumer"} {
		assert.Contains(t, subida, `"msg":"worker.started","worker":"`+nome+`"`,
			"o worker %s precisa ter subido", nome)
	}

	require.NoError(t, app.Stop(ctx), "a aplicação precisa encerrar sem erro")

	encerramento := diario.String()
	for _, nome := range []string{"reference.resolver", "outbox.publisher", "sqs.consumer"} {
		assert.Contains(t, encerramento, `"msg":"worker.stopped","worker":"`+nome+`"`,
			"o worker %s precisa ter parado", nome)
	}

	// A ordem importa: um worker ainda em pé quando o pool fecha usaria uma
	// conexão já devolvida. O Fx encerra na ordem inversa da subida, e isto
	// verifica que a ordem declarada produz esse efeito.
	assert.Less(t,
		strings.LastIndex(encerramento, `"msg":"worker.stopped"`),
		strings.Index(encerramento, `"msg":"db.closed"`),
		"todos os workers param ANTES de o banco fechar")

	// AC-6: o pool está fechado de verdade, e não apenas marcado como tal.
	assert.Error(t, db.Ping(context.Background()),
		"depois do encerramento nenhuma conexão pode continuar utilizável")
}

// AC-E3 — a composição falha rápido quando falta dependência.
//
// Distingue "não subiu" de "subiu e caiu": com o banco inalcançável, o erro
// aparece no Start, e não numa requisição qualquer minutos depois.
func TestComposicaoFalhaNaSubidaQuandoOBancoEstaInalcancavel(t *testing.T) {
	cfg := configuracaoCompleta(t)
	cfg.DB.Host = "banco-que-nao-existe.invalido"
	cfg.DB.ConnectTimeout = 3 * time.Second

	log := slog.New(slog.NewJSONHandler(&diarioSincronizado{}, nil))
	app := fx.New(
		fx.StartTimeout(time.Minute),
		fx.Supply(cfg), fx.Supply(log), fx.NopLogger,
		composition.Aplicacao,
	)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	err := app.Start(ctx)

	require.Error(t, err, "aceitar tráfego para falhar em toda requisição é pior que não subir")
	assert.NotContains(t, err.Error(), "panic")
}
