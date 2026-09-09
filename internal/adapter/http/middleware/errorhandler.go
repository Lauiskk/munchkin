package middleware

import (
	"context"
	"errors"
	"log/slog"

	"github.com/gofiber/fiber/v2"

	"github.com/Lauiskk/munchkin/internal/app"
	"github.com/Lauiskk/munchkin/pkg/apperr"
	"github.com/Lauiskk/munchkin/pkg/logs"
)

// errorBody é o corpo devolvido em qualquer falha. É contrato público.
//
// A causa e a pilha ficam de fora deliberadamente: vão para o log, onde o
// operador as alcança, e não para a resposta, onde revelariam estrutura interna
// a quem estiver sondando o serviço.
type errorBody struct {
	Code          apperr.Code         `json:"code"`
	Message       string              `json:"message"`
	Fields        []apperr.FieldError `json:"fields,omitempty"`
	CorrelationID string              `json:"correlationId,omitempty"`
}

// ErrorHandler traduz qualquer erro que chegue ao topo em resposta HTTP.
//
// Fica no adaptador, e não em pkg/apperr, para que o catálogo de erros não
// dependa do framework web — no projeto de referência da equipe o pacote de
// respostas importava Fiber, e com isso nada abaixo do handler podia ser
// testado sem uma requisição.
func ErrorHandler(log *slog.Logger) fiber.ErrorHandler {
	return func(c *fiber.Ctx, err error) error {
		appErr := translate(err)

		attrs := []slog.Attr{
			slog.String("code", string(appErr.Code)),
			slog.Int("status", appErr.HTTPStatus()),
			slog.String("method", c.Method()),
			slog.String("route", c.Route().Path),
		}

		// A causa só interessa quando a falha é nossa. Num 4xx ela seria ruído:
		// o cliente mandou algo errado, e isso não é incidente.
		if appErr.HTTPStatus() >= 500 {
			attrs = append(attrs, slog.String(logs.KeyError, err.Error()))
			log.LogAttrs(c.UserContext(), slog.LevelError, "http.error", attrs...)
		} else {
			log.LogAttrs(c.UserContext(), slog.LevelWarn, "http.rejected", attrs...)
		}

		return c.Status(appErr.HTTPStatus()).JSON(errorBody{
			Code:          appErr.Code,
			Message:       appErr.Message,
			Fields:        appErr.Fields,
			CorrelationID: correlationIDOf(c),
		})
	}
}

// translate reduz qualquer erro a um erro da aplicação.
func translate(err error) *apperr.Error {
	var appErr *apperr.Error
	if errors.As(err, &appErr) {
		return appErr
	}

	// Prazo estourado é do cliente saber: ele pode tentar de novo. Distinguir
	// isso de um 500 evita que timeout de dependência apareça como defeito.
	if errors.Is(err, context.DeadlineExceeded) {
		return apperr.Timeout("a requisição excedeu o prazo de processamento")
	}
	if errors.Is(err, context.Canceled) {
		return apperr.New(apperr.CodeTimeout, fiber.StatusRequestTimeout,
			"a requisição foi cancelada antes de concluir")
	}

	// Indisponibilidade transitória é do cliente saber: ele pode e deve tentar
	// de novo. Um 500 diria "há um defeito aqui", e defeito não se retenta.
	if errors.Is(err, app.ErrUnavailable) {
		return apperr.Unavailable("dependência temporariamente indisponível")
	}

	// Erros do próprio Fiber — rota inexistente, método não permitido, corpo
	// grande demais — já trazem o status certo; só falta o formato.
	var fiberErr *fiber.Error
	if errors.As(err, &fiberErr) {
		return apperr.New(codeForStatus(fiberErr.Code), fiberErr.Code, fiberErr.Message)
	}

	return apperr.Internal(err)
}

func codeForStatus(status int) apperr.Code {
	switch status {
	case fiber.StatusBadRequest:
		return apperr.CodeMalformedRequest
	case fiber.StatusUnauthorized:
		return apperr.CodeUnauthorized
	case fiber.StatusForbidden:
		return apperr.CodeForbidden
	case fiber.StatusNotFound:
		return apperr.CodeNotFound
	case fiber.StatusConflict:
		return apperr.CodeConflict
	case fiber.StatusTooManyRequests:
		return apperr.CodeTooManyRequests
	case fiber.StatusServiceUnavailable:
		return apperr.CodeUnavailable
	default:
		if status >= 500 {
			return apperr.CodeInternal
		}
		return apperr.CodeMalformedRequest
	}
}
