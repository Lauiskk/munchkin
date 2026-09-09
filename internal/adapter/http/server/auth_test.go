package server_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/internal/adapter/auth"
	"github.com/Lauiskk/munchkin/internal/adapter/http/middleware"
	"github.com/Lauiskk/munchkin/pkg/apperr"
)

func TestRotaProtegidaExigeCredencial(t *testing.T) {
	app := newApp(t)
	app.Get("/protegida", func(c *fiber.Ctx) error { return c.JSON(fiber.Map{"ok": true}) })

	casos := []struct {
		nome       string
		authHeader string
	}{
		{"sem cabeçalho", ""},
		{"esquema errado", "Basic dXNlcjpwYXNz"},
		{"Bearer sem token", "Bearer "},
		{"token desconhecido", "Bearer token-forjado"},
	}
	for _, caso := range casos {
		t.Run(caso.nome, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/protegida", nil)
			if caso.authHeader != "" {
				req.Header.Set(fiber.HeaderAuthorization, caso.authHeader)
			}
			resp, body := do(t, app, req)

			assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
			assert.Equal(t, string(apperr.CodeUnauthorized), body["code"])
		})
	}
}

// O esquema é insensível a caixa pela RFC 7235.
func TestEsquemaBearerEhInsensivelACaixa(t *testing.T) {
	app := newApp(t)
	app.Get("/protegida", func(c *fiber.Ctx) error { return c.JSON(fiber.Map{"ok": true}) })

	for _, esquema := range []string{"Bearer", "bearer", "BEARER", "BeArEr"} {
		req := httptest.NewRequest(http.MethodGet, "/protegida", nil)
		req.Header.Set(fiber.HeaderAuthorization, esquema+" "+tokenValido)
		resp, _ := do(t, app, req)
		assert.Equal(t, http.StatusOK, resp.StatusCode, "esquema %q", esquema)
	}
}

func TestCredencialValidaChegaAoHandlerComoIdentidade(t *testing.T) {
	app := newApp(t)

	var vista auth.Identity
	var presente bool
	app.Get("/protegida", func(c *fiber.Ctx) error {
		vista, presente = auth.FromContext(c.UserContext())
		return c.JSON(fiber.Map{"ok": true})
	})

	resp, _ := do(t, app, autenticada(httptest.NewRequest(http.MethodGet, "/protegida", nil)))

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.True(t, presente, "a identidade precisa chegar ao handler pelo context")
	assert.Equal(t, "provider-a", vista.ProviderID)
	assert.True(t, vista.IsProvider())
	assert.True(t, vista.HasScope(auth.ScopeWageringWrite))
	assert.False(t, vista.HasScope(auth.ScopeWalletsAdmin))
}

// Autenticado não é autorizado: credencial válida sem o escopo é 403, não 401.
func TestEscopoAusenteEhProibidoENaoNaoAutenticado(t *testing.T) {
	app := newApp(t)
	app.Get("/interna", middleware.RequireScope(auth.ScopeWalletsAdmin),
		func(c *fiber.Ctx) error { return c.JSON(fiber.Map{"ok": true}) })

	resp, body := do(t, app, autenticada(httptest.NewRequest(http.MethodGet, "/interna", nil)))

	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Equal(t, string(apperr.CodeForbidden), body["code"])
}

func TestEscopoPresenteLiberaAOperacao(t *testing.T) {
	app := newApp(t)
	app.Get("/envio", middleware.RequireScope(auth.ScopeWageringWrite),
		func(c *fiber.Ctx) error { return c.JSON(fiber.Map{"ok": true}) })

	resp, _ := do(t, app, autenticada(httptest.NewRequest(http.MethodGet, "/envio", nil)))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestRotaPublicaDispensaCredencial(t *testing.T) {
	app := newApp(t)
	resp, _ := do(t, app, httptest.NewRequest(http.MethodGet, "/health/live", nil))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// A comparação de rota pública é por igualdade exata, e este teste prova que
// a igualdade é o que protege: a rota protegida fica FISICAMENTE sob o caminho
// público. Com comparação por prefixo — a forma que quase todo mundo escreve —
// ela seria alcançável sem credencial. Verificado trocando IsPublic por
// strings.HasPrefix: o teste fica vermelho.
func TestCaminhoPublicoNaoServeDePrefixoParaRotaProtegida(t *testing.T) {
	app := newApp(t)
	app.Get("/health/live/interna", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"segredo": "nao deveria vazar"})
	})

	resp, body := do(t, app, httptest.NewRequest(http.MethodGet, "/health/live/interna", nil))

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode,
		"rota sob o caminho público não pode herdar a dispensa de credencial")
	assert.Equal(t, string(apperr.CodeUnauthorized), body["code"])
	assert.Nil(t, body["segredo"])
}

// O caminho que a autenticação inspeciona precisa ser o mesmo que o roteador
// usa para casar a rota. Se divergissem, um caminho poderia parecer público
// para a autenticação e casar uma rota protegida no roteamento.
//
// A requisição vai autenticada de propósito: sem credencial a autenticação
// corta antes, e a sonda nunca observaria o caminho — que foi como a primeira
// versão deste teste se enganou.
func TestCaminhoInspecionadoEhOMesmoQueRoteia(t *testing.T) {
	app := newApp(t)

	var vistoDepoisDaAutenticacao string
	app.Use(func(c *fiber.Ctx) error {
		vistoDepoisDaAutenticacao = c.Path()
		return c.Next()
	})
	app.Get("/protegida", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"alcancou": true})
	})

	for _, alvo := range []string{
		"/health/live/../protegida",
		"/health/live/../../protegida",
		"/health/live%2f..%2fprotegida",
		"/health/live;/protegida",
		"/health//live",
	} {
		t.Run(alvo, func(t *testing.T) {
			vistoDepoisDaAutenticacao = ""
			resp, body := do(t, app, autenticada(httptest.NewRequest(http.MethodGet, alvo, nil)))

			// O Fiber entrega o caminho cru, sem normalizar, e o roteador casa
			// pelo mesmo caminho cru. É essa concordância que fecha a brecha.
			assert.Equal(t, alvo, vistoDepoisDaAutenticacao,
				"a autenticação precisa ver o mesmo caminho que o roteador usa")
			assert.Equal(t, http.StatusNotFound, resp.StatusCode,
				"%q não pode casar a rota protegida", alvo)
			assert.Nil(t, body["alcancou"])
		})
	}
}
