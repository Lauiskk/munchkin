package event_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/internal/domain/event"
	"github.com/Lauiskk/munchkin/internal/domain/money"
	"github.com/Lauiskk/munchkin/internal/domain/wagering"
	"github.com/Lauiskk/munchkin/internal/domain/wallet"
)

var agora = time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

func conteudo(t *testing.T) event.WagerTransactionProcessed {
	t.Helper()
	wid, err := wallet.NewID()
	require.NoError(t, err)
	pid, err := wallet.NewPlayerID()
	require.NoError(t, err)
	tid, err := wagering.NewTransactionID()
	require.NoError(t, err)
	valor, err := money.Parse("25.00", money.BRL)
	require.NoError(t, err)
	saldo, err := money.Parse("975.00", money.BRL)
	require.NoError(t, err)

	return event.WagerTransactionProcessed{
		TransactionID: tid, WalletID: wid, PlayerID: pid,
		Kind: wagering.Bet, Money: valor, Balance: saldo,
		ProviderID: "provider-a", ExternalID: "tx-1",
		ProcessedAt: event.Timestamp(agora),
	}
}

// AC-12: tipo e versão vêm do payload, não de parâmetro. É o que impede um
// evento ser publicado com o tipo de outro.
func TestTipoEVersaoVemDoConteudoNaoDoChamador(t *testing.T) {
	id, err := event.NewID()
	require.NoError(t, err)
	c := conteudo(t)

	env, err := event.New(id, c, "corr-1", "caus-1", agora)
	require.NoError(t, err)

	assert.Equal(t, event.TypeWagerTransactionProcessed, env.Type())
	assert.Equal(t, 1, env.Version())
	assert.Equal(t, c.WalletID, env.AggregateID())
	assert.Equal(t, event.AggregateWallet, env.AggregateType())
	assert.Equal(t, id, env.ID())
	assert.Equal(t, "corr-1", env.CorrelationID())
	assert.Equal(t, "caus-1", env.CausationID())
	assert.Equal(t, agora, env.OccurredAt())
}

func TestCadaEventoDeclaraOProprioTipo(t *testing.T) {
	wid, err := wallet.NewID()
	require.NoError(t, err)

	esperado := map[event.Type]event.Payload{
		event.TypeWagerTransactionProcessed:        event.WagerTransactionProcessed{WalletID: wid},
		event.TypeWagerTransactionRejected:         event.WagerTransactionRejected{WalletID: wid},
		event.TypeWagerTransactionPendingReference: event.WagerTransactionPendingReference{WalletID: wid},
		event.TypeWalletBalanceChanged:             event.WalletBalanceChanged{WalletID: wid},
	}
	for tipo, p := range esperado {
		assert.Equal(t, tipo, p.EventType())
		assert.Equal(t, 1, p.EventVersion())
		assert.Equal(t, wid, p.AggregateID())
	}
	assert.Len(t, esperado, 4, "os quatro eventos exigidos pelo enunciado")
}

func TestEnvelopeRecusaConstrucaoIncompleta(t *testing.T) {
	id, err := event.NewID()
	require.NoError(t, err)
	c := conteudo(t)

	t.Run("sem identificador", func(t *testing.T) {
		_, err := event.New(event.ID{}, c, "", "", agora)
		assert.ErrorIs(t, err, event.ErrInvalidID)
	})

	t.Run("sem conteúdo", func(t *testing.T) {
		_, err := event.New(id, nil, "", "", agora)
		assert.ErrorIs(t, err, event.ErrInvalidEnvelope)
	})

	// Agregado ausente significa evento que o publicador não sabe particionar —
	// e a ordenação por carteira na fila depende dele.
	t.Run("sem agregado", func(t *testing.T) {
		_, err := event.New(id, event.WalletBalanceChanged{}, "", "", agora)
		assert.ErrorIs(t, err, event.ErrInvalidEnvelope)
	})
}

// A correlação é opcional: um evento gerado por worker, sem requisição HTTP por
// trás, não tem de inventar uma.
func TestCorrelacaoEhOpcional(t *testing.T) {
	id, err := event.NewID()
	require.NoError(t, err)

	env, err := event.New(id, conteudo(t), "", "", agora)
	require.NoError(t, err)

	assert.Empty(t, env.CorrelationID())
	assert.Empty(t, env.CausationID())
}

func TestInstanteEhNormalizadoParaUTC(t *testing.T) {
	id, err := event.NewID()
	require.NoError(t, err)
	saoPaulo := time.FixedZone("BRT", -3*60*60)

	env, err := event.New(id, conteudo(t), "", "", agora.In(saoPaulo))
	require.NoError(t, err)

	assert.Equal(t, time.UTC, env.OccurredAt().Location())
	assert.Equal(t, agora, env.OccurredAt())
}
