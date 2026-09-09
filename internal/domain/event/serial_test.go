package event_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/internal/domain/event"
	"github.com/Lauiskk/munchkin/internal/domain/money"
	"github.com/Lauiskk/munchkin/internal/domain/wagering"
	"github.com/Lauiskk/munchkin/internal/domain/wallet"
)

// Um identificador que saia como array de bytes no JSON é ilegível para o
// consumidor e impede qualquer correlação. Como os tipos de identificador são
// definidos sobre uuid.UUID — que é [16]byte —, eles NÃO herdam a serialização
// textual: sem método próprio, o evento publicado sai com "walletId":[1,2,...].
func TestIdentificadoresSaemComoTextoNoJSON(t *testing.T) {
	wid, err := wallet.NewID()
	require.NoError(t, err)
	pid, err := wallet.NewPlayerID()
	require.NoError(t, err)
	tid, err := wagering.NewTransactionID()
	require.NoError(t, err)
	eid, err := event.NewID()
	require.NoError(t, err)

	corpo, err := json.Marshal(map[string]any{
		"walletId": wid, "playerId": pid, "transactionId": tid, "eventId": eid,
	})
	require.NoError(t, err)

	var lido map[string]any
	require.NoError(t, json.Unmarshal(corpo, &lido))

	for campo, esperado := range map[string]string{
		"walletId": wid.String(), "playerId": pid.String(),
		"transactionId": tid.String(), "eventId": eid.String(),
	} {
		valor, ok := lido[campo].(string)
		require.True(t, ok, "%s precisa ser string, veio %T", campo, lido[campo])
		assert.Equal(t, esperado, valor)
	}
}

func TestIdentificadoresVoltamDoJSON(t *testing.T) {
	wid, err := wallet.NewID()
	require.NoError(t, err)

	corpo, err := json.Marshal(wid)
	require.NoError(t, err)

	var lido wallet.ID
	require.NoError(t, json.Unmarshal(corpo, &lido))
	assert.Equal(t, wid, lido)
}

func TestEventoDeSaldoCarregaOsCamposExigidos(t *testing.T) {
	wid, _ := wallet.NewID()
	tid, _ := wagering.NewTransactionID()
	antes, err := money.Parse("100.00", money.BRL)
	require.NoError(t, err)
	depois, err := money.Parse("20.00", money.BRL)
	require.NoError(t, err)
	valor, err := money.Parse("80.00", money.BRL)
	require.NoError(t, err)

	corpo, err := json.Marshal(event.WalletBalanceChanged{
		WalletID: wid, TransactionID: tid, Direction: wallet.Debit,
		Money: valor, BalanceBefore: antes, BalanceAfter: depois,
		WalletVersion: 2, ChangedAt: event.Timestamp(time.Now().UTC()),
	})
	require.NoError(t, err)

	var lido map[string]any
	require.NoError(t, json.Unmarshal(corpo, &lido))

	// O §11 nomeia exatamente estes campos.
	for _, campo := range []string{
		"walletId", "transactionId", "direction", "money",
		"balanceBefore", "balanceAfter", "walletVersion",
	} {
		assert.Contains(t, lido, campo)
	}

	// E o dinheiro tem de sair no formato do contrato, com o valor em string.
	assert.Equal(t, map[string]any{"amount": "80.00", "currency": "BRL"}, lido["money"])
}
