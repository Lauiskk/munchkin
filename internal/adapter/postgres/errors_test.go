package postgres

import (
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Uma constraint declarada tem nome, e o nome é o que o caso de uso consulta
// para distinguir "carteira já existe" de "operação já registrada".
func TestErroDeConstraintCarregaONome(t *testing.T) {
	err := classify(&pgconn.PgError{
		Code: sqlstateUniqueViolation, ConstraintName: ConstraintWalletPlayerCurrency,
		Message: "duplicate key value violates unique constraint",
	})

	nome, ok := ConstraintNameOf(err)
	require.True(t, ok)
	assert.Equal(t, ConstraintWalletPlayerCurrency, nome)
	assert.Contains(t, err.Error(), ConstraintWalletPlayerCurrency)
}

// Gatilho que recusa por RAISE não preenche nome de constraint nem de tabela.
// Sem a saída pelo erro de baixo, a mensagem seria "invariante do banco
// violada:" e nada mais — que é o que se vê quando os livros não fecham, e é
// justamente quando o diagnóstico importa.
func TestErroDeGatilhoSemNomeMostraAMensagemDoServidor(t *testing.T) {
	err := classify(&pgconn.PgError{
		Code:    sqlstateCheckViolation,
		Message: "os livros nao fecham na transacao 01a0: sobra de -100 em unidades minimas",
	})

	assert.Contains(t, err.Error(), "os livros nao fecham")
	assert.Contains(t, err.Error(), "sobra de -100")

	nome, ok := ConstraintNameOf(err)
	assert.True(t, ok, "continua sendo violação de invariante")
	assert.Empty(t, nome, "e não há nome de constraint para consultar")
}
