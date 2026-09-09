package dto_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/internal/adapter/http/dto"
	"github.com/Lauiskk/munchkin/internal/domain/money"
	"github.com/Lauiskk/munchkin/pkg/apperr"
)

const jogador = "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1"

func decodifica(t *testing.T, corpo string) (dto.OpenWalletRequest, error) {
	t.Helper()
	var req dto.OpenWalletRequest
	require.NoError(t, json.Unmarshal([]byte(corpo), &req))
	_, _, err := req.Decode()
	return req, err
}

func TestAberturaValidaEhAceita(t *testing.T) {
	var req dto.OpenWalletRequest
	require.NoError(t, json.Unmarshal([]byte(`{
		"playerId": "`+jogador+`",
		"initialBalance": {"amount": "1000.00", "currency": "BRL"}
	}`), &req))

	playerID, saldo, err := req.Decode()
	require.NoError(t, err)

	assert.Equal(t, jogador, playerID.String())
	assert.Equal(t, "1000.00", saldo.Amount())
	assert.Equal(t, money.BRL, saldo.Currency())
}

func TestSaldoZeroEhAceito(t *testing.T) {
	var req dto.OpenWalletRequest
	require.NoError(t, json.Unmarshal([]byte(`{
		"playerId": "`+jogador+`",
		"initialBalance": {"amount": "0.00", "currency": "BRL"}
	}`), &req))

	_, saldo, err := req.Decode()
	require.NoError(t, err)
	assert.True(t, saldo.IsZero())
}

// Acumular os erros em vez de parar no primeiro é deliberado: quem integra
// corrige tudo de uma vez, em vez de descobrir um problema por tentativa.
func TestErrosDeCampoSaoAcumulados(t *testing.T) {
	_, err := decodifica(t, `{
		"playerId": "nao-e-uuid",
		"initialBalance": {"amount": "25.0", "currency": "XYZ"}
	}`)
	require.Error(t, err)

	var appErr *apperr.Error
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, apperr.CodeValidation, appErr.Code)

	campos := map[string]bool{}
	for _, f := range appErr.Fields {
		campos[f.Field] = true
	}
	assert.True(t, campos["playerId"], "o jogador inválido precisa ser apontado")
	assert.True(t, campos["initialBalance.currency"], "a moeda inválida precisa ser apontada")
}

func TestCadaEntradaInvalidaApontaOCampo(t *testing.T) {
	casos := map[string]struct {
		corpo string
		campo string
	}{
		"jogador ausente":    {`{"playerId":"","initialBalance":{"amount":"1.00","currency":"BRL"}}`, "playerId"},
		"jogador malformado": {`{"playerId":"abc","initialBalance":{"amount":"1.00","currency":"BRL"}}`, "playerId"},
		"moeda desconhecida": {`{"playerId":"` + jogador + `","initialBalance":{"amount":"1.00","currency":"XYZ"}}`, "initialBalance.currency"},
		"moeda em minúscula": {`{"playerId":"` + jogador + `","initialBalance":{"amount":"1.00","currency":"brl"}}`, "initialBalance.currency"},
		"escala errada":      {`{"playerId":"` + jogador + `","initialBalance":{"amount":"25.0","currency":"BRL"}}`, "initialBalance.amount"},
		"escala excedente":   {`{"playerId":"` + jogador + `","initialBalance":{"amount":"25.999","currency":"BRL"}}`, "initialBalance.amount"},
		"valor vazio":        {`{"playerId":"` + jogador + `","initialBalance":{"amount":"","currency":"BRL"}}`, "initialBalance.amount"},
		"notação científica": {`{"playerId":"` + jogador + `","initialBalance":{"amount":"1e3","currency":"BRL"}}`, "initialBalance.amount"},
		"saldo negativo":     {`{"playerId":"` + jogador + `","initialBalance":{"amount":"-1.00","currency":"BRL"}}`, "initialBalance.amount"},
	}
	for nome, caso := range casos {
		t.Run(nome, func(t *testing.T) {
			_, err := decodifica(t, caso.corpo)
			require.Error(t, err)

			var appErr *apperr.Error
			require.ErrorAs(t, err, &appErr)
			require.NotEmpty(t, appErr.Fields)

			encontrado := false
			for _, f := range appErr.Fields {
				if f.Field == caso.campo {
					encontrado = true
				}
			}
			assert.True(t, encontrado, "esperava o campo %q apontado, veio %+v", caso.campo, appErr.Fields)
		})
	}
}

// O identificador da carteira não faz parte do corpo. Aceitá-lo permitiria
// colisão deliberada e sondagem de identificadores existentes.
func TestCorpoNaoAceitaIdentificadorDeCarteira(t *testing.T) {
	corpo, err := json.Marshal(dto.OpenWalletRequest{})
	require.NoError(t, err)
	assert.NotContains(t, string(corpo), `"id"`)

	var req dto.OpenWalletRequest
	require.NoError(t, json.Unmarshal([]byte(`{
		"id": "0192f291-27dd-7d3f-8071-5f8685deef37",
		"playerId": "`+jogador+`",
		"initialBalance": {"amount": "1.00", "currency": "BRL"}
	}`), &req))

	// O campo simplesmente não existe na struct: o valor enviado é descartado.
	playerID, _, err := req.Decode()
	require.NoError(t, err)
	assert.Equal(t, jogador, playerID.String())
}
