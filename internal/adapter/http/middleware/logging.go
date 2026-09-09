package middleware

import (
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
)

// Logging registra uma linha por requisição.
//
// Registra método, rota, status e duração — nunca corpo. O enunciado proíbe
// registrar payload financeiro completo, e a forma segura de cumprir isso é o
// log nunca ter acesso ao corpo, em vez de tentar limpá-lo depois.
//
// Os health checks são registrados em debug: com sondagem a cada poucos
// segundos, eles afogariam qualquer coisa útil no nível padrão.
func Logging(log *slog.Logger) fiber.Handler {
	return func(c *fiber.Ctx) error {
		start := time.Now()
		err := c.Next()

		// O tratador de erro do Fiber só escreve a resposta depois que a cadeia
		// de middleware desenrola, então o status final vem do erro quando há um.
		//
		// A tradução é a MESMA usada pelo tratador de erro, de propósito. Na
		// primeira versão este trecho chamava apperr.From, que não reconhece
		// *fiber.Error: um 404 era respondido como 404 ao cliente e registrado
		// como 500 no log. Log que discorda da resposta é pior que log nenhum,
		// porque manda o plantonista investigar um incidente que não houve.
		status := c.Response().StatusCode()
		if err != nil {
			status = translate(err).HTTPStatus()
		}

		level := slog.LevelInfo
		switch {
		case strings.HasPrefix(c.Path(), "/health/"):
			level = slog.LevelDebug
		case status >= 500:
			level = slog.LevelError
		case status >= 400:
			level = slog.LevelWarn
		}

		log.LogAttrs(c.UserContext(), level, "http.request",
			slog.String("method", c.Method()),
			slog.String("route", c.Route().Path),
			slog.String("path", c.Path()),
			slog.Int("status", status),
			slog.Duration("duration", time.Since(start)),
		)
		return err
	}
}
