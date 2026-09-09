// Package contract carrega a forma de fio das operações financeiras, comum a
// todos os transportes.
//
// Não mora sob `http/` porque não é de HTTP: a mesma operação entra por
// requisição e por mensagem, e o §8 do enunciado exige que as duas produzam o
// mesmo hash de idempotência. Duas cópias da mesma validação divergem, e a
// divergência apareceria justamente ali — como duas operações distintas para o
// que é a mesma.
package contract

import (
	"errors"
	"strings"

	"github.com/Lauiskk/munchkin/pkg/apperr"
)

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

// Merge incorpora os erros de campo de outro erro de validação, prefixando os
// nomes. É o que permite reaproveitar um decode aninhado sem perder de vista
// onde, no documento recebido, o campo problemático estava.
func (f *Fields) Merge(prefixo string, err error) {
	var v *apperr.Error
	if !errors.As(err, &v) || len(v.Fields) == 0 {
		f.Add(strings.TrimSuffix(prefixo, "."), err.Error(), apperr.FieldCodeInvalidValue)
		return
	}
	for _, campo := range v.Fields {
		campo.Field = prefixo + campo.Field
		f.erros = append(f.erros, campo)
	}
}
