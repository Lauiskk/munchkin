// Package apperr concentra os erros da aplicação e sua tradução para HTTP.
//
// O desenho vem do projeto de referência da equipe, com três correções:
//
//  1. o erro implementa Unwrap, então errors.Is e errors.As atravessam a cadeia
//     — no original a causa era carregada como campo solto e a cadeia se perdia;
//  2. a causa e a pilha nunca são serializadas, só registradas em log;
//  3. o corpo devolvido carrega o identificador de correlação, para que o
//     cliente consiga citar a ocorrência exata ao abrir um chamado.
package apperr

import (
	"errors"
	"fmt"
	"net/http"
)

// Code é o código estável devolvido ao cliente. É contrato: uma vez publicado,
// muda de significado nunca.
type Code string

const (
	CodeMalformedRequest Code = "MALFORMED_REQUEST"
	CodeValidation       Code = "VALIDATION_ERROR"
	CodeUnauthorized     Code = "UNAUTHORIZED"
	CodeForbidden        Code = "FORBIDDEN"
	CodeNotFound         Code = "NOT_FOUND"
	CodeConflict         Code = "CONFLICT"
	CodeBusinessRejected Code = "BUSINESS_REJECTED"
	CodeTooManyRequests  Code = "TOO_MANY_REQUESTS"
	CodeTimeout          Code = "TIMEOUT"
	CodeUnavailable      Code = "SERVICE_UNAVAILABLE"
	CodeInternal         Code = "INTERNAL_ERROR"
	// CodeReportLimitExceeded — o relatório existe, os dados estão íntegros, e
	// o total pedido não cabe no tipo em que ele é expresso. Separado de
	// INTERNAL_ERROR porque a ação de quem recebe é outra: não é "abra um
	// chamado", é "os totais passaram do que este relatório sabe representar".
	CodeReportLimitExceeded Code = "REPORT_LIMIT_EXCEEDED"
)

// FieldError descreve um campo rejeitado na validação da entrada.
type FieldError struct {
	Field    string `json:"field"`
	Message  string `json:"message"`
	Code     string `json:"code"`
	Expected any    `json:"expected,omitempty"`
	Received any    `json:"received,omitempty"`
}

// Códigos de validação por campo.
const (
	FieldCodeRequired      = "MISSING_FIELD"
	FieldCodeInvalidFormat = "INVALID_FORMAT"
	FieldCodeInvalidValue  = "INVALID_VALUE"
	FieldCodeInvalidLength = "INVALID_LENGTH"
)

// Error é o erro da aplicação. O status HTTP e a causa não são exportados:
// status porque é detalhe de transporte que só o tratador precisa ler, causa
// porque não pode vazar para o cliente.
type Error struct {
	Code    Code
	Message string
	Fields  []FieldError

	status int
	cause  error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap expõe a causa para errors.Is e errors.As.
func (e *Error) Unwrap() error { return e.cause }

// Is considera dois erros equivalentes quando têm o mesmo código, o que permite
// errors.Is(err, apperr.NotFound("")) sem comparar mensagem.
func (e *Error) Is(target error) bool {
	var t *Error
	if !errors.As(target, &t) {
		return false
	}
	return t.Code == e.Code
}

// HTTPStatus devolve o status de transporte correspondente.
func (e *Error) HTTPStatus() int { return e.status }

// WithCause anexa a causa. Ela é registrada em log e nunca serializada.
func (e *Error) WithCause(cause error) *Error {
	e.cause = cause
	return e
}

// WithFields anexa erros por campo.
func (e *Error) WithFields(fields ...FieldError) *Error {
	e.Fields = append(e.Fields, fields...)
	return e
}

// New monta um erro com código, status e mensagem explícitos. Prefira os
// construtores nomeados abaixo; este existe para códigos de negócio próprios.
func New(code Code, status int, message string) *Error {
	return &Error{Code: code, Message: message, status: status}
}

// Malformed indica corpo, parâmetro ou cabeçalho que não pôde ser interpretado.
func Malformed(message string) *Error {
	return New(CodeMalformedRequest, http.StatusBadRequest, message)
}

// Validation indica entrada sintaticamente válida mas semanticamente recusada.
func Validation(fields ...FieldError) *Error {
	return New(CodeValidation, http.StatusBadRequest,
		"a requisição contém campos inválidos").WithFields(fields...)
}

// Unauthorized indica credencial ausente, inválida ou expirada.
func Unauthorized(message string) *Error {
	return New(CodeUnauthorized, http.StatusUnauthorized, message)
}

// Forbidden indica credencial válida sem permissão para a operação.
func Forbidden(message string) *Error {
	return New(CodeForbidden, http.StatusForbidden, message)
}

// NotFound indica recurso inexistente ou fora do alcance do solicitante.
func NotFound(message string) *Error {
	return New(CodeNotFound, http.StatusNotFound, message)
}

// Conflict indica choque com o estado atual — chave reutilizada com conteúdo
// diferente, recurso já existente.
func Conflict(message string) *Error {
	return New(CodeConflict, http.StatusConflict, message)
}

// Rejected indica recusa por regra de negócio. É distinto de Validation: a
// entrada estava correta, a operação é que não pode acontecer.
func Rejected(message string) *Error {
	return New(CodeBusinessRejected, http.StatusUnprocessableEntity, message)
}

// TooManyRequests indica excesso de requisições.
func TooManyRequests(message string) *Error {
	return New(CodeTooManyRequests, http.StatusTooManyRequests, message)
}

// Timeout indica prazo de execução esgotado.
func Timeout(message string) *Error {
	return New(CodeTimeout, http.StatusGatewayTimeout, message)
}

// Unavailable indica indisponibilidade transitória de uma dependência. É
// deliberadamente distinto de Internal: o cliente pode e deve tentar de novo.
func Unavailable(message string) *Error {
	return New(CodeUnavailable, http.StatusServiceUnavailable, message)
}

// Internal indica falha inesperada. A mensagem devolvida é sempre genérica; o
// que aconteceu de fato vai para o log, com a causa.
func Internal(cause error) *Error {
	return New(CodeInternal, http.StatusInternalServerError,
		"erro inesperado ao processar a requisição").WithCause(cause)
}

// ReportLimitExceeded indica total de relatório fora da faixa representável.
// A mensagem é explícita de propósito: a condição é conhecida e entendida, e
// devolvê-la como "erro inesperado" esconderia isso de quem opera.
func ReportLimitExceeded(message string) *Error {
	return New(CodeReportLimitExceeded, http.StatusInternalServerError, message)
}

// From converte um erro qualquer em *Error, preservando o que já for da
// aplicação e traduzindo o que for reconhecível.
func From(err error) *Error {
	if err == nil {
		return nil
	}
	var appErr *Error
	if errors.As(err, &appErr) {
		return appErr
	}
	return Internal(err)
}

// CodeOf devolve o código do erro, ou CodeInternal quando não for da aplicação.
func CodeOf(err error) Code {
	var appErr *Error
	if errors.As(err, &appErr) {
		return appErr.Code
	}
	return CodeInternal
}
