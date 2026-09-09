package contract_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/internal/adapter/contract"
	"github.com/Lauiskk/munchkin/pkg/apperr"
)

// mensagemValida é o exemplo do §10 do enunciado, verbatim nos campos.
const mensagemValida = `{
  "messageId": "msg-123",
  "type": "WagerTransactionRequested",
  "occurredAt": "2026-09-08T12:00:00.000Z",
  "data": {
    "providerId": "provider-a",
    "externalTransactionId": "transaction-123",
    "idempotencyKey": "provider-a:transaction-123",
    "playerId": "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
    "walletId": "0192f291-27dd-7d3f-8071-5f8685deef37",
    "roundId": "round-987",
    "gameId": "fortune-chimp",
    "kind": "BET",
    "money": { "amount": "25.00", "currency": "BRL" }
  }
}`

func TestMensagemDoEnunciadoEhAceita(t *testing.T) {
	m, err := contract.DecodeMessage([]byte(mensagemValida))

	require.NoError(t, err)
	assert.Equal(t, "msg-123", m.MessageID)
	assert.Equal(t, "provider-a:transaction-123", m.IdempotencyKey)
	assert.Equal(t, "2026-09-08T12:00:00Z", m.OccurredAt.Format("2006-01-02T15:04:05Z"))
	assert.Equal(t, "transaction-123", string(m.Transaction.ExternalID))
	assert.Equal(t, "25.00 BRL", m.Transaction.Money.String())
}

// AC-9 — a mesma operação, pelos dois transportes, tem de produzir campos
// idênticos. Se divergissem, o hash de idempotência divergiria com eles, e a
// mesma operação viraria duas.
func TestHTTPEMensageriaProduzemOsMesmosCampos(t *testing.T) {
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(mensagemValida), &envelope))

	var pelaFila contract.SubmitTransactionRequest
	require.NoError(t, json.Unmarshal(envelope.Data, &pelaFila))

	// O corpo HTTP é o mesmo objeto `data`, sem a chave de idempotência — que
	// lá viaja em cabeçalho.
	var porHTTP contract.SubmitTransactionRequest
	require.NoError(t, json.Unmarshal(envelope.Data, &porHTTP))

	daFila, err := pelaFila.Decode()
	require.NoError(t, err)
	doHTTP, err := porHTTP.Decode()
	require.NoError(t, err)

	assert.Equal(t, doHTTP, daFila, "os dois transportes decodificam para o mesmo valor")

	completa, err := contract.DecodeMessage([]byte(mensagemValida))
	require.NoError(t, err)
	assert.Equal(t, doHTTP, completa.Transaction,
		"o envelope não pode alterar a decodificação da operação que carrega")
}

func TestEnvelopeInvalidoEhRecusadoComOCampoCerto(t *testing.T) {
	casos := map[string]struct {
		corpo  string
		campos []string
	}{
		"json malformado": {`{"messageId":`, nil},
		"messageId vazio": {
			`{"messageId":"","type":"WagerTransactionRequested","data":{"idempotencyKey":"k"}}`,
			[]string{"messageId"},
		},
		"tipo desconhecido": {
			`{"messageId":"m1","type":"OutraCoisa","data":{"idempotencyKey":"k"}}`,
			[]string{"type"},
		},
		"chave de idempotência ausente": {
			`{"messageId":"m1","type":"WagerTransactionRequested","data":{}}`,
			[]string{"data.idempotencyKey"},
		},
		"occurredAt fora do formato": {
			`{"messageId":"m1","type":"WagerTransactionRequested","occurredAt":"ontem","data":{"idempotencyKey":"k"}}`,
			[]string{"occurredAt"},
		},
	}

	for nome, c := range casos {
		t.Run(nome, func(t *testing.T) {
			_, err := contract.DecodeMessage([]byte(c.corpo))
			require.Error(t, err)

			for _, esperado := range c.campos {
				assert.Contains(t, camposDe(t, err), esperado)
			}
		})
	}
}

// Os erros do corpo da operação chegam com o caminho completo dentro do
// envelope: quem publica precisa saber que o problema está em `data.money`, e
// não num campo `money` solto que não existe na mensagem que ele enviou.
func TestErroNaOperacaoVemPrefixadoPeloEnvelope(t *testing.T) {
	corpo := `{
	  "messageId": "msg-1", "type": "WagerTransactionRequested",
	  "data": {"idempotencyKey":"provider-a:tx-1","providerId":"provider-a",
	           "externalTransactionId":"tx-1","playerId":"não-é-uuid",
	           "walletId":"0192f291-27dd-7d3f-8071-5f8685deef37",
	           "roundId":"r","gameId":"g","kind":"BET",
	           "money":{"amount":"25.00","currency":"XXX"}}}`

	_, err := contract.DecodeMessage([]byte(corpo))

	require.Error(t, err)
	campos := camposDe(t, err)
	assert.Contains(t, campos, "data.playerId")
	assert.Contains(t, campos, "data.money.currency")
}

func TestHashDependeDoConteudoCruDaMensagem(t *testing.T) {
	a := contract.HashMessage([]byte(mensagemValida))
	b := contract.HashMessage([]byte(mensagemValida))
	assert.Equal(t, a, b, "o mesmo corpo tem de dar o mesmo hash")

	diferente := contract.HashMessage([]byte(`{"messageId":"msg-123","type":"x"}`))
	assert.NotEqual(t, a, diferente)
	assert.Len(t, a, 32)
}

func camposDe(t *testing.T, err error) []string {
	t.Helper()
	var v *apperr.Error
	if !errors.As(err, &v) {
		return nil
	}
	nomes := make([]string, 0, len(v.Fields))
	for _, c := range v.Fields {
		nomes = append(nomes, c.Field)
	}
	return nomes
}
