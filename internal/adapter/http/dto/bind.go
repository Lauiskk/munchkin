// Package dto traduz entre o formato de fio e o domínio.
//
// Toda validação de entrada externa acontece aqui, na fronteira. O que passa
// daqui já é tipo de domínio validado, e nenhuma camada adiante precisa
// reconferir — nem tem como esquecer de conferir.
package dto

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gofiber/fiber/v2"

	"github.com/Lauiskk/munchkin/pkg/apperr"
)

// maxBodyBytes limita o corpo aceito.
//
// Os corpos deste serviço têm algumas centenas de bytes. Aceitar mais permitiria
// que um cliente consumisse memória e tempo do processo antes de qualquer
// validação de negócio acontecer.
const maxBodyBytes = 16 * 1024

// Bind lê o corpo JSON para dst, traduzindo falhas em erro com o campo apontado.
//
// O erro de tipo do encoding/json carrega o caminho do campo. Aproveitá-lo dá
// uma mensagem útil — "o campo initialBalance.amount deveria ser texto" — em vez
// do genérico "corpo inválido", que obriga quem integra a adivinhar.
func Bind(c *fiber.Ctx, dst any) error {
	corpo := c.Body()
	if len(corpo) == 0 {
		return apperr.Malformed("corpo da requisição ausente")
	}
	if len(corpo) > maxBodyBytes {
		return apperr.Malformed(fmt.Sprintf(
			"corpo com %d bytes excede o máximo de %d", len(corpo), maxBodyBytes))
	}

	if err := json.Unmarshal(corpo, dst); err != nil {
		var tipoErrado *json.UnmarshalTypeError
		if errors.As(err, &tipoErrado) {
			campo := tipoErrado.Field
			if campo == "" {
				campo = "(raiz)"
			}
			return apperr.Validation(apperr.FieldError{
				Field:    campo,
				Message:  fmt.Sprintf("esperava %s", tipoErrado.Type.String()),
				Code:     apperr.FieldCodeInvalidFormat,
				Expected: tipoErrado.Type.String(),
				Received: tipoErrado.Value,
			})
		}
		return apperr.Malformed("o corpo não é um JSON válido")
	}
	return nil
}

// Fields acumula erros por campo e os converte em erro de validação.
//
// Acumular em vez de parar no primeiro é deliberado: quem integra corrige tudo
// de uma vez, em vez de descobrir um problema por tentativa.
type Fields struct {
	erros []apperr.FieldError
}

// Add registra um campo inválido.
func (f *Fields) Add(campo, mensagem, codigo string) {
	f.erros = append(f.erros, apperr.FieldError{
		Field: campo, Message: mensagem, Code: codigo,
	})
}

// AddErr registra um campo inválido usando a mensagem de um erro.
func (f *Fields) AddErr(campo string, err error, codigo string) {
	f.Add(campo, err.Error(), codigo)
}

// Err devolve o erro de validação, ou nil se não houve problema.
func (f *Fields) Err() error {
	if len(f.erros) == 0 {
		return nil
	}
	return apperr.Validation(f.erros...)
}
