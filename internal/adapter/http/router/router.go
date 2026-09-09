// Package router declara as rotas da aplicação.
package router

import (
	"github.com/gofiber/fiber/v2"

	"github.com/Lauiskk/munchkin/internal/adapter/http/handler"
)

// publicPaths são as rotas que dispensam autenticação.
//
// É um conjunto de caminhos exatos, e não uma lista de prefixos, de propósito:
// com prefixo, "/health/live/../../wallets" passaria como rota pública. A
// comparação exata não tem essa brecha.
var publicPaths = map[string]struct{}{
	"/health/live":  {},
	"/health/ready": {},
}

// PublicPaths é o predicado que decide se uma rota dispensa autenticação.
// Injetado em vez de referenciado direto para que os testes possam exercitar a
// cadeia com outra política sem mexer na de produção.
type PublicPaths func(path string) bool

// NewPublicPaths devolve a política de produção.
func NewPublicPaths() PublicPaths { return IsPublic }

// IsPublic informa se o caminho dispensa autenticação.
func IsPublic(path string) bool {
	_, ok := publicPaths[path]
	return ok
}

// Register declara as rotas no aplicativo.
func Register(app *fiber.App, health *handler.Health) {
	app.Get("/health/live", health.Live)
	app.Get("/health/ready", health.Ready)
}
