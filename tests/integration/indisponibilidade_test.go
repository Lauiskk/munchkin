//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/Lauiskk/munchkin/internal/config"
)

// bancoDerrubavel sobe um PostgreSQL que o teste pode matar no meio do caminho.
//
// É o único jeito honesto de exercitar indisponibilidade transitória: simular
// com um dublê provaria que o dublê devolve o que eu mandei devolver, não que o
// driver e a classificação reconhecem um banco que caiu.
func bancoDerrubavel(t *testing.T) (config.DB, func()) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	script := caminhoDoScriptDePapeis(t)
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
			wait.ForAll(
				wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
				wait.ForListeningPort("5432/tcp"),
			).WithStartupTimeoutDefault(2*time.Minute)),
	)
	require.NoError(t, err, "Docker precisa estar em execução")

	derrubar := func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = container.Terminate(c)
	}
	t.Cleanup(derrubar)

	host, err := container.Host(ctx)
	require.NoError(t, err)
	porta, err := container.MappedPort(ctx, "5432/tcp")
	require.NoError(t, err)

	return config.DB{
		Host: host, Port: int(porta.Num()), Name: testDBName,
		User: testAppUser, Password: config.Secret(testAppPassword),
		SSLMode: "disable", MaxOpenConns: 5, MaxIdleConns: 2,
		ConnMaxLifetime: 30 * time.Minute, ConnMaxIdleTime: 5 * time.Minute,
		ConnectTimeout: 5 * time.Second,
	}, derrubar
}

// O §9 exige que indisponibilidade transitória seja distinguível pelo contrato.
//
// A distinção não é cosmética: 500 diz "há um defeito aqui" e não se retenta;
// 503 diz "tente de novo". Um banco que caiu por trinta segundos aparecendo
// como defeito da aplicação faz quem integra abrir chamado em vez de repetir a
// requisição.
func TestBancoIndisponivelRespondeRetentavelENaoDefeito(t *testing.T) {
	dbCfg, derrubar := bancoDerrubavel(t)

	a := novoAmbienteHTTPCom(t, dbCfg)
	jogador := jogadorNovo(t)
	corpoAbertura := fmt.Sprintf(
		`{"playerId":%q,"initialBalance":{"amount":"100.00","currency":"BRL"}}`, jogador)

	resp, corpo := a.chamar(t, http.MethodPost, "/wallets", corpoAbertura, admin(), nil)
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(corpo))

	// O banco cai com a aplicação de pé — que é como isso acontece de verdade.
	derrubar()

	resp, corpo = a.chamar(t, http.MethodPost, "/wallets", corpoAbertura, admin(), nil)

	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode,
		"banco fora do ar é 'tente de novo', não 'defeito nosso': %s", string(corpo))
	assert.Contains(t, string(corpo), "SERVICE_UNAVAILABLE")
	a.conferir(t, "/wallets", http.MethodPost, http.StatusServiceUnavailable, corpo)
}
