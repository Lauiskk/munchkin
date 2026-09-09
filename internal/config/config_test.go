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

func TestPadroesQuandoOAmbienteEstaVazio(t *testing.T) {
	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, config.EnvLocal, cfg.App.Env)
	assert.Equal(t, 8080, cfg.HTTP.Port)
	assert.Equal(t, config.LogFormatJSON, cfg.Log.Format)
	assert.Equal(t, ":8080", cfg.HTTP.Addr())
	assert.False(t, cfg.App.IsProduction())
}

func TestLeituraDoAmbiente(t *testing.T) {
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

func TestPortaForaDaFaixa(t *testing.T) {
	for _, porta := range []string{"0", "65536", "-1"} {
		t.Run(porta, func(t *testing.T) {
			t.Setenv("HTTP_PORT", porta)
			_, err := config.Load()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "HTTP_PORT")
		})
	}
}

func TestDuracaoNaoPodeSerZeroOuNegativa(t *testing.T) {
	t.Setenv("HTTP_READ_TIMEOUT", "0s")
	_, err := config.Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "maior que zero")
}

// Se o prazo da requisição não couber na janela de escrita, o servidor corta a
// resposta antes de o handler desistir e o cliente recebe conexão encerrada em
// vez do erro de timeout — que é muito mais difícil de diagnosticar.
func TestPrazoDaRequisicaoPrecisaCaberNaJanelaDeEscrita(t *testing.T) {
	t.Setenv("HTTP_WRITE_TIMEOUT", "5s")
	t.Setenv("HTTP_REQUEST_TIMEOUT", "10s")

	_, err := config.Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP_REQUEST_TIMEOUT")
	assert.Contains(t, err.Error(), "HTTP_WRITE_TIMEOUT")
}
