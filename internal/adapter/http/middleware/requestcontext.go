package middleware

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/Lauiskk/munchkin/pkg/correlation"
)

// RequestContext monta o context.Context da aplicação para a requisição e o
// instala com SetUserContext.
//
// Este é o único ponto do projeto que lê o contexto do fasthttp. Fiber não usa
// net/http, então o context.Context de aplicação não chega pronto ao handler:
// ele é montado aqui, uma vez, com prazo de execução e identificador de
// correlação. Todo o resto do código usa c.UserContext(), e há um gate de CI
// que recusa quem tentar o contrário.
func RequestContext(timeout time.Duration) fiber.Handler {
	return func(c *fiber.Ctx) error {
		base := c.Context() // gate:allow-fiber-ctx

		ctx, cancel := context.WithTimeout(base, timeout)
		defer cancel()

		c.SetUserContext(correlation.Into(ctx, correlationIDOf(c)))
		return c.Next()
	}
}
