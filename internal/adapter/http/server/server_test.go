package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/internal/adapter/auth"
	"github.com/Lauiskk/munchkin/internal/adapter/http/handler"
	"github.com/Lauiskk/munchkin/internal/adapter/http/router"
	"github.com/Lauiskk/munchkin/internal/adapter/http/server"
	"github.com/Lauiskk/munchkin/internal/config"
	"github.com/Lauiskk/munchkin/pkg/apperr"
	"github.com/Lauiskk/munchkin/pkg/correlation"
)

// tokenValido é aceito pelo verificador de teste; qualquer outro é recusado.
const tokenValido = "token-de-teste-valido"

func newApp(t *testing.T, checks ...handler.ReadinessCheck) *fiber.App {
	t.Helper()
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cfg := config.Config{
		App:  config.App{Env: config.EnvTest},
		HTTP: config.HTTP{RequestTimeout: 2 * time.Second},
	}
	// O handler de carteira entra sem casos de uso: estes testes exercitam a
	// cadeia de middleware, não as rotas de carteira. E elas exigem escopo
	// wallets:admin, que o verificador de teste não concede — uma chamada
	// acidental para em 403, antes de alcançar o handler.
	return server.New(cfg, log, handler.NewHealth(log, time.Second, checks...),
		handler.NewWallet(nil, nil), handler.NewWagering(nil, nil),
		verificadorDeTeste{}, router.IsPublic)
}

// autenticada acrescenta uma credencial válida à requisição.
func autenticada(req *http.Request) *http.Request {
	req.Header.Set(fiber.HeaderAuthorization, "Bearer "+tokenValido)
	return req
}

// verificadorDeTeste substitui o IdP na exercitação da cadeia de middleware.
// A validação real de token, contra um Keycloak de verdade, está nos testes de
// integração — aqui o que se testa é a cadeia, não a criptografia.
type verificadorDeTeste struct{}

func (verificadorDeTeste) Verify(_ context.Context, raw string) (auth.Identity, error) {
	if raw != tokenValido {
		return auth.Identity{}, auth.ErrTokenInvalid
	}
	return auth.NewIdentity("sub-teste", "provider-a", "provider-a",
		auth.ScopeWageringRead, auth.ScopeWageringWrite), nil
}

func do(t *testing.T, app *fiber.App, req *http.Request) (*http.Response, map[string]any) {
	t.Helper()
	resp, err := app.Test(req, 5000)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var decoded map[string]any
	if len(body) > 0 {
		require.NoError(t, json.Unmarshal(body, &decoded), "corpo: %s", body)
	}
	return resp, decoded
}

func TestLivenessNaoDependeDeNada(t *testing.T) {
	// A sonda de vivacidade responde mesmo com toda dependência falhando: se
	// ela caísse junto com o banco, o orquestrador reiniciaria a aplicação
	// inteira durante uma indisponibilidade do banco.
	app := newApp(t, failingCheck{name: "postgres"})

	resp, body := do(t, app, httptest.NewRequest(http.MethodGet, "/health/live", nil))

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "alive", body["status"])
}

func TestReadinessRefleteAsDependencias(t *testing.T) {
	t.Run("todas saudáveis", func(t *testing.T) {
		app := newApp(t, okCheck{name: "postgres"}, okCheck{name: "sqs"})
		resp, body := do(t, app, httptest.NewRequest(http.MethodGet, "/health/ready", nil))

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "ready", body["status"])
		assert.Equal(t, map[string]any{"postgres": "ok", "sqs": "ok"}, body["checks"])
	})

	t.Run("uma falhando", func(t *testing.T) {
		app := newApp(t, okCheck{name: "sqs"}, failingCheck{name: "postgres"})
		resp, body := do(t, app, httptest.NewRequest(http.MethodGet, "/health/ready", nil))

		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		assert.Equal(t, "not_ready", body["status"])
		assert.Equal(t, "failing", body["checks"].(map[string]any)["postgres"])
	})
}

// O endpoint de prontidão é público. Dizer QUAL dependência falhou é operação;
// dizer POR QUE vazaria host, usuário e às vezes credencial.
func TestReadinessNaoVazaOMotivoDaFalha(t *testing.T) {
	app := newApp(t, failingCheck{
		name: "postgres",
		err:  errors.New("dial tcp 10.0.0.5:5432: password authentication failed for user \"munchkin\""),
	})

	resp, _ := do(t, app, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

	raw, err := app.Test(httptest.NewRequest(http.MethodGet, "/health/ready", nil), 5000)
	require.NoError(t, err)
	defer func() { _ = raw.Body.Close() }()
	corpo, err := io.ReadAll(raw.Body)
	require.NoError(t, err)

	assert.NotContains(t, string(corpo), "10.0.0.5")
	assert.NotContains(t, string(corpo), "password")
	assert.NotContains(t, string(corpo), "munchkin")
}

// Um pânico no handler não pode derrubar o processo nem vazar a pilha.
func TestPanicoNoHandlerViraQuinhentosSemVazarPilha(t *testing.T) {
	app := newApp(t)
	app.Get("/estoura", func(*fiber.Ctx) error {
		panic("índice fora do intervalo em algum lugar profundo")
	})

	resp, body := do(t, app, autenticada(httptest.NewRequest(http.MethodGet, "/estoura", nil)))

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Equal(t, string(apperr.CodeInternal), body["code"])
	assert.NotContains(t, body["message"], "índice fora do intervalo",
		"a mensagem do pânico não pode chegar ao cliente")
	assert.NotContains(t, body["message"], "goroutine")
	assert.NotEmpty(t, body["correlationId"], "mesmo em pânico a resposta traz correlação")

	// E o mais importante: a aplicação continua atendendo.
	resp2, body2 := do(t, app, httptest.NewRequest(http.MethodGet, "/health/live", nil))
	assert.Equal(t, http.StatusOK, resp2.StatusCode)
	assert.Equal(t, "alive", body2["status"])
}

// Sem credencial, uma rota inexistente responde 401 e não 404. É deliberado:
// 404 contaria a um chamador anônimo quais caminhos existem, e o mapa de uma
// API financeira não é informação pública.
func TestRotaInexistenteNaoRevelaSuaAusenciaAoAnonimo(t *testing.T) {
	app := newApp(t)
	resp, body := do(t, app, httptest.NewRequest(http.MethodGet, "/nao-existe", nil))

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Equal(t, string(apperr.CodeUnauthorized), body["code"])
	assert.NotEmpty(t, body["correlationId"])
}

func TestRotaInexistenteRespondeQuatrocentosEQuatroParaAutenticado(t *testing.T) {
	app := newApp(t)
	resp, body := do(t, app, autenticada(httptest.NewRequest(http.MethodGet, "/nao-existe", nil)))

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Equal(t, string(apperr.CodeNotFound), body["code"])
}

func TestErroDaAplicacaoPreservaCodigoEStatus(t *testing.T) {
	app := newApp(t)
	app.Get("/rejeitado", func(*fiber.Ctx) error {
		return apperr.Rejected("saldo insuficiente").
			WithCause(errors.New("detalhe interno que não pode vazar"))
	})

	resp, body := do(t, app, autenticada(httptest.NewRequest(http.MethodGet, "/rejeitado", nil)))

	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Equal(t, string(apperr.CodeBusinessRejected), body["code"])
	assert.Equal(t, "saldo insuficiente", body["message"])
	assert.NotContains(t, body["message"], "detalhe interno")
}

func TestCorrelacaoEhDevolvidaEValidada(t *testing.T) {
	app := newApp(t)

	t.Run("aproveita o identificador do cliente", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/health/live", nil)
		req.Header.Set(correlation.HeaderName, "pedido-abc-123")
		resp, _ := do(t, app, req)
		assert.Equal(t, "pedido-abc-123", resp.Header.Get(correlation.HeaderName))
	})

	// Aceitar a string crua permitiria injetar quebra de linha e forjar
	// registros no log estruturado.
	t.Run("descarta tentativa de injeção", func(t *testing.T) {
		for _, malicioso := range []string{
			`abc"} {"forjado":"sim`,
			"abc\nlinha-forjada",
			"a-muito-longo-" + string(make([]byte, 120)),
		} {
			req := httptest.NewRequest(http.MethodGet, "/health/live", nil)
			req.Header.Set(correlation.HeaderName, malicioso)
			resp, _ := do(t, app, req)

			devolvido := resp.Header.Get(correlation.HeaderName)
			assert.NotEqual(t, malicioso, devolvido)
			assert.NotContains(t, devolvido, `"`)
			assert.NotContains(t, devolvido, "\n")
		}
	})
}

// O prazo da requisição precisa chegar ao handler pelo context.
func TestContextDaRequisicaoCarregaPrazoECorrelacao(t *testing.T) {
	app := newApp(t)

	var temPrazo bool
	var idNoContext string
	app.Get("/inspeciona", func(c *fiber.Ctx) error {
		ctx := c.UserContext()
		_, temPrazo = ctx.Deadline()
		idNoContext = correlation.From(ctx)
		return c.JSON(fiber.Map{"ok": true})
	})

	req := autenticada(httptest.NewRequest(http.MethodGet, "/inspeciona", nil))
	req.Header.Set(correlation.HeaderName, "abc-123")
	resp, _ := do(t, app, req)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.True(t, temPrazo, "o context do handler precisa carregar prazo de execução")
	assert.Equal(t, "abc-123", idNoContext)
}

type okCheck struct{ name string }

func (c okCheck) Name() string                { return c.name }
func (c okCheck) Check(context.Context) error { return nil }

type failingCheck struct {
	name string
	err  error
}

func (c failingCheck) Name() string { return c.name }
func (c failingCheck) Check(context.Context) error {
	if c.err != nil {
		return c.err
	}
	return errors.New("indisponível")
}
