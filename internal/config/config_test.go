package config_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/internal/config"
)

// comObrigatorios define o mínimo sem o qual a aplicação não sobe, para que os
// demais testes exercitem o que de fato querem exercitar.
func comObrigatorios(t *testing.T) {
	t.Helper()
	t.Setenv("AUTH_ISSUER", "http://localhost:8180/realms/munchkin")
	t.Setenv("AUTH_AUDIENCE", "munchkin-api")
	t.Setenv("DB_HOST", "localhost")
	t.Setenv("DB_NAME", "munchkin")
	t.Setenv("DB_USER", "munchkin_app")
	t.Setenv("DB_PASSWORD", "local-only-app")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "local-only-access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "local-only-secret-key")
	t.Setenv("AWS_EVENTS_QUEUE_URL", "http://localstack:4566/000000000000/wager-events.fifo")
	t.Setenv("AWS_TRANSACTIONS_QUEUE_URL", "http://localstack:4566/000000000000/wager-transactions.fifo")
	t.Setenv("AWS_TRANSACTIONS_DLQ_URL", "http://localstack:4566/000000000000/wager-transactions-dlq.fifo")
}

func TestPadroesQuandoOAmbienteEstaVazio(t *testing.T) {
	comObrigatorios(t)

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, config.EnvLocal, cfg.App.Env)
	assert.Equal(t, 8080, cfg.HTTP.Port)
	assert.Equal(t, config.LogFormatJSON, cfg.Log.Format)
	assert.Equal(t, ":8080", cfg.HTTP.Addr())
	assert.False(t, cfg.App.IsProduction())
	assert.Equal(t, 15*time.Minute, cfg.Auth.JWKSRefreshInterval)
	assert.Equal(t, time.Minute, cfg.Auth.JWKSMinRefreshInterval)
	assert.Equal(t, 10, cfg.DB.MaxOpenConns)
	assert.Equal(t, 5, cfg.DB.MaxIdleConns)
	assert.Equal(t, 5*time.Second, cfg.Worker.ReferenceInterval)
}

// Mais conexões ociosas que abertas é configuração sem sentido, e quase sempre
// significa que os dois valores foram trocados de lugar.
func TestOciosasNaoPodemExcederAsAbertas(t *testing.T) {
	comObrigatorios(t)
	t.Setenv("DB_MAX_OPEN_CONNS", "5")
	t.Setenv("DB_MAX_IDLE_CONNS", "20")

	_, err := config.Load()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "DB_MAX_IDLE_CONNS")
}

// Sem emissor e audiência a aplicação não pode subir: um serviço que não sabe
// em qual IdP confiar aceitaria tráfego sem conseguir validar credencial, e
// ausência de autenticação efetiva nos endpoints de negócio é eliminatória.
func TestAutenticacaoEhObrigatoria(t *testing.T) {
	t.Setenv("DB_HOST", "localhost")
	t.Setenv("DB_NAME", "munchkin")
	t.Setenv("DB_USER", "munchkin_app")
	t.Setenv("DB_PASSWORD", "local-only-app")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "local-only-access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "local-only-secret-key")
	t.Setenv("AWS_EVENTS_QUEUE_URL", "http://localstack:4566/000000000000/wager-events.fifo")
	t.Setenv("AWS_TRANSACTIONS_QUEUE_URL", "http://localstack:4566/000000000000/wager-transactions.fifo")
	t.Setenv("AWS_TRANSACTIONS_DLQ_URL", "http://localstack:4566/000000000000/wager-transactions-dlq.fifo")

	_, err := config.Load()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "AUTH_ISSUER")
	assert.Contains(t, err.Error(), "AUTH_AUDIENCE")
}

// A URL de descoberta pode diferir do emissor: dentro da rede do compose a
// aplicação alcança o IdP por um endereço interno, mas o token continua sendo
// emitido com o endereço público.
func TestDescobertaPodeApontarParaEnderecoInterno(t *testing.T) {
	comObrigatorios(t)
	t.Setenv("AUTH_DISCOVERY_URL",
		"http://keycloak:8080/realms/munchkin/.well-known/openid-configuration")

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, "http://localhost:8180/realms/munchkin", cfg.Auth.Issuer)
	assert.Equal(t, "http://keycloak:8080/realms/munchkin/.well-known/openid-configuration",
		cfg.Auth.DiscoveryURL)
}

func TestLeituraDoAmbiente(t *testing.T) {
	comObrigatorios(t)
	t.Setenv("APP_ENV", config.EnvProduction)
	t.Setenv("HTTP_PORT", "9999")
	t.Setenv("HTTP_REQUEST_TIMEOUT", "2s")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("LOG_FORMAT", config.LogFormatText)

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.True(t, cfg.App.IsProduction())
	assert.Equal(t, 9999, cfg.HTTP.Port)
	assert.Equal(t, 2*time.Second, cfg.HTTP.RequestTimeout)
	assert.Equal(t, "debug", cfg.Log.Level)
	assert.Equal(t, config.LogFormatText, cfg.Log.Format)
}

// Falhar no primeiro problema obrigaria a subir, corrigir e subir de novo, uma
// variável por vez. O relatório tem que listar tudo de uma vez.
func TestTodosOsProblemasSaoReportadosJuntos(t *testing.T) {
	comObrigatorios(t)
	t.Setenv("APP_ENV", "homologacao")
	t.Setenv("HTTP_PORT", "não-é-número")
	t.Setenv("HTTP_READ_TIMEOUT", "quinze segundos")
	t.Setenv("LOG_LEVEL", "verboso")

	_, err := config.Load()
	require.Error(t, err)
	assert.True(t, errors.Is(err, config.ErrInvalid))

	msg := err.Error()
	for _, chave := range []string{"APP_ENV", "HTTP_PORT", "HTTP_READ_TIMEOUT", "LOG_LEVEL"} {
		assert.Contains(t, msg, chave, "o relatório deve citar %s", chave)
	}
	assert.Equal(t, 4, strings.Count(msg, "\n  - "), "um problema por linha")
}

// Sem banco a aplicação também não sobe.
func TestBancoEhObrigatorio(t *testing.T) {
	t.Setenv("AUTH_ISSUER", "http://localhost:8180/realms/munchkin")
	t.Setenv("AUTH_AUDIENCE", "munchkin-api")

	_, err := config.Load()

	require.Error(t, err)
	for _, chave := range []string{"DB_HOST", "DB_NAME", "DB_USER", "DB_PASSWORD"} {
		assert.Contains(t, err.Error(), chave)
	}
}

func TestPortaForaDaFaixa(t *testing.T) {
	for _, porta := range []string{"0", "65536", "-1"} {
		t.Run(porta, func(t *testing.T) {
			comObrigatorios(t)
			t.Setenv("HTTP_PORT", porta)
			_, err := config.Load()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "HTTP_PORT")
		})
	}
}

func TestDuracaoNaoPodeSerZeroOuNegativa(t *testing.T) {
	comObrigatorios(t)
	t.Setenv("HTTP_READ_TIMEOUT", "0s")
	_, err := config.Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "maior que zero")
}

// Se o prazo da requisição não couber na janela de escrita, o servidor corta a
// resposta antes de o handler desistir e o cliente recebe conexão encerrada em
// vez do erro de timeout — que é muito mais difícil de diagnosticar.
func TestPrazoDaRequisicaoPrecisaCaberNaJanelaDeEscrita(t *testing.T) {
	comObrigatorios(t)
	t.Setenv("HTTP_WRITE_TIMEOUT", "5s")
	t.Setenv("HTTP_REQUEST_TIMEOUT", "10s")

	_, err := config.Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP_REQUEST_TIMEOUT")
	assert.Contains(t, err.Error(), "HTTP_WRITE_TIMEOUT")
}
