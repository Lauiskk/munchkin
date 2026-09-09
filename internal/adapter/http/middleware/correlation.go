// Package middleware reúne os interceptadores HTTP da aplicação.
package middleware

import (
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/Lauiskk/munchkin/pkg/correlation"
)

// maxCorrelationIDLen limita o identificador aceito do cliente.
const maxCorrelationIDLen = 64

// Correlation resolve o identificador de correlação da requisição e o devolve
// no cabeçalho da resposta.
//
// Um identificador vindo do cliente é aproveitado — é assim que se rastreia uma
// operação através de vários serviços — mas só depois de validado. Aceitar a
// string crua permitiria injetar quebra de linha e conteúdo arbitrário no log
// estruturado, forjando registros. Fora do formato esperado, geramos o nosso.
func Correlation() fiber.Handler {
	return func(c *fiber.Ctx) error {
		id := strings.TrimSpace(c.Get(correlation.HeaderName))
		if !validCorrelationID(id) {
			id = correlation.New()
		}
		c.Locals(correlation.LocalsKey, id)
		c.Set(correlation.HeaderName, id)
		return c.Next()
	}
}

// validCorrelationID aceita apenas caracteres seguros para log e cabeçalho.
func validCorrelationID(id string) bool {
	if id == "" || len(id) > maxCorrelationIDLen {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// correlationIDOf lê o identificador dos Locals.
func correlationIDOf(c *fiber.Ctx) string {
	id, _ := c.Locals(correlation.LocalsKey).(string)
	return id
}
