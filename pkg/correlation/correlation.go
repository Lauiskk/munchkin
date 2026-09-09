// Package correlation carrega o identificador de correlação da operação pelo
// context.Context, para que log, resposta de erro e evento publicado possam ser
// amarrados à mesma requisição.
package correlation

import (
	"context"

	"github.com/google/uuid"
)

// HeaderName é o cabeçalho aceito e devolvido nas respostas HTTP.
const HeaderName = "X-Correlation-ID"

// LocalsKey é a chave usada nos Locals do Fiber, para que o tratador de erro
// alcance o identificador sem depender do context.
const LocalsKey = "correlationID"

type ctxKey struct{}

// New gera um identificador novo. Usa UUID versão 7, que é ordenável por tempo
// e, portanto, agrupa naturalmente as operações de um mesmo período quando os
// logs são ordenados por esse campo.
func New() string {
	id, err := uuid.NewV7()
	if err != nil {
		// NewV7 só falha se a fonte de entropia falhar. Um identificador de
		// correlação ausente não pode derrubar a requisição, então caímos para
		// a versão 4, que usa o mesmo gerador mas não depende do relógio.
		return uuid.NewString()
	}
	return id.String()
}

// Into devolve um context derivado carregando o identificador.
func Into(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, id)
}

// From extrai o identificador do context. Devolve string vazia quando ausente:
// a ausência de correlação nunca é erro, apenas menos informação no log.
func From(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}
