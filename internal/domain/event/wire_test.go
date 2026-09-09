package event_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/internal/domain/event"
	"github.com/Lauiskk/munchkin/internal/domain/money"
	"github.com/Lauiskk/munchkin/internal/domain/wallet"
)

func TestTimestampSaiEmUTCComPrecisaoFixa(t *testing.T) {
	saoPaulo, err := time.LoadLocation("America/Sao_Paulo")
	require.NoError(t, err)

	casos := map[string]struct {
		entrada  time.Time
		esperado string
	}{
		"instante redondo": {
			time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC),
			"2026-09-09T12:00:00.000Z",
		},
		"com milissegundos": {
			time.Date(2026, 9, 9, 12, 0, 0, 120_000_000, time.UTC),
			"2026-09-09T12:00:00.120Z",
		},
		"precisão além do milissegundo é truncada": {
			time.Date(2026, 9, 9, 12, 0, 0, 120_999_999, time.UTC),
			"2026-09-09T12:00:00.120Z",
		},
		"outro fuso é convertido, não anotado": {
			time.Date(2026, 9, 9, 9, 0, 0, 0, saoPaulo),
			"2026-09-09T12:00:00.000Z",
		},
	}

	for nome, c := range casos {
		t.Run(nome, func(t *testing.T) {
			assert.Equal(t, c.esperado, event.FormatTimestamp(c.entrada))
		})
	}
}

func TestWireCarregaOsCamposDoContrato(t *testing.T) {
	id, err := event.NewID()
	require.NoError(t, err)

	bruto, err := json.Marshal(event.Wire{
		EventID:       id,
		EventType:     event.TypeWalletBalanceChanged,
		AggregateType: "wallet",
		AggregateID:   "0192f291-0000-7000-8000-000000000000",
		CorrelationID: "01a083fe-0000-7000-8000-000000000000",
		OccurredAt:    event.FormatTimestamp(time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)),
		Version:       1,
		Data:          json.RawMessage(`{"walletId":"0192f291-0000-7000-8000-000000000000"}`),
	})
	require.NoError(t, err)

	var visto map[string]any
	require.NoError(t, json.Unmarshal(bruto, &visto))

	// Os nove campos do §11. Um campo a menos quebra o consumidor; um campo a
	// mais entra sem quebrar, e por isso a lista é conferida por igualdade.
	esperados := []string{
		"eventId", "eventType", "aggregateType", "aggregateId",
		"correlationId", "causationId", "occurredAt", "version", "data",
	}
	assert.Len(t, visto, len(esperados))
	for _, campo := range esperados {
		assert.Contains(t, visto, campo)
	}

	assert.Nil(t, visto["causationId"], "causationId é opcional e sai como null, não some")
	assert.Equal(t, id.String(), visto["eventId"])
	assert.Equal(t, "2026-09-09T12:00:00.000Z", visto["occurredAt"])
}

func TestTimestampNoPayloadSegueOMesmoFormatoDoEnvelope(t *testing.T) {
	valor, err := money.Parse("10.00", money.BRL)
	require.NoError(t, err)

	bruto, err := json.Marshal(event.WalletBalanceChanged{
		Direction:     wallet.Credit,
		Money:         valor,
		BalanceBefore: valor,
		BalanceAfter:  valor,
		WalletVersion: 2,
		ChangedAt:     event.Timestamp(time.Date(2026, 9, 9, 12, 0, 0, 761_848_186, time.UTC)),
	})
	require.NoError(t, err)
	assert.Contains(t, string(bruto), `"changedAt":"2026-09-09T12:00:00.761Z"`,
		"instante do payload não pode sair com precisão diferente da do envelope")
}

// Um evento gravado antes da normalização continua na outbox com precisão de
// nanossegundos. Se a leitura recusasse aquele formato, a mudança de contrato
// teria transformado o histórico em lixo.
func TestTimestampLeQualquerPrecisaoDeRFC3339(t *testing.T) {
	casos := []string{
		`"2026-09-09T12:00:00Z"`,
		`"2026-09-09T12:00:00.761Z"`,
		`"2026-09-09T12:00:00.761848186Z"`,
		`"2026-09-09T09:00:00-03:00"`,
	}

	for _, entrada := range casos {
		var lido event.Timestamp
		require.NoError(t, json.Unmarshal([]byte(entrada), &lido), entrada)
		assert.Equal(t, 2026, lido.Time().Year())
		assert.Equal(t, time.UTC, lido.Time().Location(), "a leitura normaliza para UTC")
	}
}
