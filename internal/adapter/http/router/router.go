// Package router declara as rotas da aplicação.
package router

import (
	"github.com/gofiber/fiber/v2"

	"github.com/Lauiskk/munchkin/internal/adapter/auth"
	"github.com/Lauiskk/munchkin/internal/adapter/http/handler"
	"github.com/Lauiskk/munchkin/internal/adapter/http/middleware"
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
//
// O escopo exigido fica na tabela de rotas, ao lado do caminho. É o único lugar
// onde dá para ler, de uma vez, quem pode chamar o quê — espalhado pelos
// handlers, sempre existe um que ninguém lembra de proteger.
func Register(
	app *fiber.App,
	health *handler.Health,
	wallets *handler.Wallet,
	wagers *handler.Wagering,
) {
	app.Get("/health/live", health.Live)
	app.Get("/health/ready", health.Ready)

	// Abertura e consulta de carteira são operações internas: o §2 do enunciado
	// restringe operações de carteira ao serviço interno, e um provedor com
	// wagering:write recebe 403 aqui.
	app.Post("/wallets", middleware.RequireScope(auth.ScopeWalletsAdmin), wallets.Open)
	app.Get("/wallets/:walletId", middleware.RequireScope(auth.ScopeWalletsAdmin), wallets.Get)

	// Operações financeiras: escrita e leitura têm escopos distintos, para que
	// um integrador que só consulta não precise de credencial que movimenta.
	app.Post("/wagering/transactions",
		middleware.RequireScope(auth.ScopeWageringWrite), wagers.Submit)
	app.Get("/wagering/transactions/:transactionId",
		middleware.RequireScope(auth.ScopeWageringRead), wagers.GetByID)
	app.Get("/providers/:providerId/wagering/transactions/:externalTransactionId",
		middleware.RequireScope(auth.ScopeWageringRead), wagers.GetByExternalID)
}
