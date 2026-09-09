// Package router declara as rotas da aplicação.
package router

import (
	"github.com/gofiber/fiber/v2"

	"github.com/Lauiskk/munchkin/internal/adapter/http/handler"
)

// PublicPaths são as rotas que dispensam autenticação.
//
// É um conjunto de caminhos exatos, e não uma lista de prefixos, de propósito:
// com prefixo, "/health/live/../../wallets" passaria como rota pública. A
// comparação exata não tem essa brecha.
var PublicPaths = map[string]struct{}{
	"/health/live":  {},
	"/health/ready": {},
}

// IsPublic informa se o caminho dispensa autenticação.
func IsPublic(path string) bool {
	_, ok := PublicPaths[path]
	return ok
}

// Register declara as rotas no aplicativo.
func Register(app *fiber.App, health *handler.Health) {
	app.Get("/health/live", health.Live)
	app.Get("/health/ready", health.Ready)
}
