package apperr_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/pkg/apperr"
)

func TestConstrutoresMapeiamStatusHTTP(t *testing.T) {
	tests := []struct {
		name       string
		err        *apperr.Error
		wantCode   apperr.Code
		wantStatus int
	}{
		{"malformado", apperr.Malformed("corpo ilegível"), apperr.CodeMalformedRequest, http.StatusBadRequest},
		{"validação", apperr.Validation(), apperr.CodeValidation, http.StatusBadRequest},
		{"não autenticado", apperr.Unauthorized("sem credencial"), apperr.CodeUnauthorized, http.StatusUnauthorized},
		{"sem permissão", apperr.Forbidden("escopo ausente"), apperr.CodeForbidden, http.StatusForbidden},
		{"inexistente", apperr.NotFound("carteira"), apperr.CodeNotFound, http.StatusNotFound},
		{"conflito", apperr.Conflict("chave reutilizada"), apperr.CodeConflict, http.StatusConflict},
		{"rejeição de negócio", apperr.Rejected("saldo insuficiente"), apperr.CodeBusinessRejected, http.StatusUnprocessableEntity},
		{"indisponível", apperr.Unavailable("banco fora"), apperr.CodeUnavailable, http.StatusServiceUnavailable},
		{"interno", apperr.Internal(errors.New("x")), apperr.CodeInternal, http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantCode, tt.err.Code)
			assert.Equal(t, tt.wantStatus, tt.err.HTTPStatus())
		})
	}
}

// Entrada inválida e rejeição de negócio precisam ser distinguíveis pelo
// contrato: uma o cliente corrige, a outra não.
func TestValidacaoEhDistintaDeRejeicaoDeNegocio(t *testing.T) {
	validacao := apperr.Validation()
	rejeicao := apperr.Rejected("saldo insuficiente")

	assert.NotEqual(t, validacao.Code, rejeicao.Code)
	assert.NotEqual(t, validacao.HTTPStatus(), rejeicao.HTTPStatus())
}

func TestErrorsAsAtravessaACadeia(t *testing.T) {
	raiz := errors.New("conexão recusada")
	wrapped := fmt.Errorf("ao abrir transação: %w", apperr.Unavailable("banco fora").WithCause(raiz))

	var appErr *apperr.Error
	require.True(t, errors.As(wrapped, &appErr))
	assert.Equal(t, apperr.CodeUnavailable, appErr.Code)

	// Unwrap tem que alcançar a causa original, senão a classificação por
	// errors.Is se perde assim que alguém embrulha o erro mais uma vez.
	assert.True(t, errors.Is(wrapped, raiz))
}

func TestIsComparaPorCodigo(t *testing.T) {
	err := apperr.NotFound("carteira 123")

	assert.True(t, errors.Is(err, apperr.NotFound("qualquer outra mensagem")))
	assert.False(t, errors.Is(err, apperr.Conflict("outro código")))
}

// A causa carrega detalhe de infraestrutura e não pode chegar ao cliente. O
// que a protege é ela não ser exportada — este teste registra a intenção.
func TestCausaNaoEhSerializada(t *testing.T) {
	segredo := errors.New("dial tcp 10.0.0.5:5432: connection refused")
	err := apperr.Internal(segredo)

	assert.NotContains(t, err.Message, "10.0.0.5")
	assert.Contains(t, err.Error(), "10.0.0.5", "a causa deve continuar visível para o log")
}

func TestFromPreservaErroDaAplicacaoEEmbrulhaODesconhecido(t *testing.T) {
	original := apperr.Conflict("já existe")
	assert.Same(t, original, apperr.From(fmt.Errorf("contexto: %w", original)))

	convertido := apperr.From(errors.New("qualquer coisa"))
	assert.Equal(t, apperr.CodeInternal, convertido.Code)

	assert.Nil(t, apperr.From(nil))
}

func TestCodeOfClassificaErroDesconhecidoComoInterno(t *testing.T) {
	assert.Equal(t, apperr.CodeNotFound, apperr.CodeOf(apperr.NotFound("x")))
	assert.Equal(t, apperr.CodeInternal, apperr.CodeOf(errors.New("x")))
}

func TestWithFieldsAcumula(t *testing.T) {
	err := apperr.Validation(
		apperr.FieldError{Field: "money.amount", Code: apperr.FieldCodeInvalidFormat},
	).WithFields(apperr.FieldError{Field: "kind", Code: apperr.FieldCodeInvalidValue})

	require.Len(t, err.Fields, 2)
	assert.Equal(t, "money.amount", err.Fields[0].Field)
	assert.Equal(t, "kind", err.Fields[1].Field)
}
