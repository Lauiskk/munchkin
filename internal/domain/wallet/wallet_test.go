package wallet_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/internal/domain/money"
	"github.com/Lauiskk/munchkin/internal/domain/wallet"
)

var agora = time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

func brl(t *testing.T, v string) money.Money {
	t.Helper()
	m, err := money.Parse(v, money.BRL)
	require.NoError(t, err)
	return m
}

func ids(t *testing.T) (wallet.ID, wallet.PlayerID) {
	t.Helper()
	id, err := wallet.NewID()
	require.NoError(t, err)
	p, err := wallet.NewPlayerID()
	require.NoError(t, err)
	return id, p
}

func aberta(t *testing.T, saldo string) *wallet.Wallet {
	t.Helper()
	id, p := ids(t)
	w, _, err := wallet.Open(id, p, brl(t, saldo), agora)
	require.NoError(t, err)
	return w
}

// AC-1
func TestAberturaComSaldoPositivo(t *testing.T) {
	id, p := ids(t)

	w, mv, err := wallet.Open(id, p, brl(t, "1000.00"), agora)
	require.NoError(t, err)

	assert.Equal(t, id, w.ID())
	assert.Equal(t, p, w.PlayerID())
	assert.Equal(t, money.BRL, w.Currency())
	assert.Equal(t, "1000.00", w.Balance().Amount())

	// O crédito de abertura faz parte da criação, não é movimentação posterior:
	// por isso a versão é 1, e não 2.
	assert.Equal(t, int64(1), w.Version())

	require.NotNil(t, mv, "abertura com saldo positivo produz a movimentação de crédito")
	assert.Equal(t, wallet.Credit, mv.Direction)
	assert.Equal(t, "0.00", mv.BalanceBefore.Amount())
	assert.Equal(t, "1000.00", mv.BalanceAfter.Amount())
	assert.Equal(t, int64(1), mv.Version)
}

// AC-2
func TestAberturaComSaldoZeroNaoProduzMovimentacao(t *testing.T) {
	id, p := ids(t)

	w, mv, err := wallet.Open(id, p, brl(t, "0.00"), agora)
	require.NoError(t, err)

	assert.Nil(t, mv, "saldo inicial zero não gera lançamento nem evento financeiro")
	assert.Equal(t, int64(1), w.Version())
	assert.True(t, w.Balance().IsZero())
}

func TestAberturaRecusaEntradaInvalida(t *testing.T) {
	id, p := ids(t)

	t.Run("identificador de carteira ausente", func(t *testing.T) {
		_, _, err := wallet.Open(wallet.ID{}, p, brl(t, "10.00"), agora)
		assert.ErrorIs(t, err, wallet.ErrInvalidID)
	})
	t.Run("identificador de jogador ausente", func(t *testing.T) {
		_, _, err := wallet.Open(id, wallet.PlayerID{}, brl(t, "10.00"), agora)
		assert.ErrorIs(t, err, wallet.ErrInvalidID)
	})
	t.Run("saldo não inicializado", func(t *testing.T) {
		_, _, err := wallet.Open(id, p, money.Money{}, agora)
		assert.ErrorIs(t, err, money.ErrUninitialized)
	})
	t.Run("saldo inicial negativo", func(t *testing.T) {
		negativo, err := brl(t, "10.00").Neg()
		require.NoError(t, err)
		_, _, err = wallet.Open(id, p, negativo, agora)
		assert.ErrorIs(t, err, wallet.ErrInvalidState)
	})
}

// AC-3: reidratar devolve o estado, não o reconstrói. Confundir reidratação com
// criação é como um saldo é creditado duas vezes.
func TestReidratacaoNaoProduzMovimentacao(t *testing.T) {
	id, p := ids(t)

	w, err := wallet.Rehydrate(id, p, brl(t, "975.00"), 7, agora, agora)
	require.NoError(t, err)

	assert.Equal(t, "975.00", w.Balance().Amount())
	assert.Equal(t, int64(7), w.Version(), "a versão persistida é preservada")
}

func TestReidratacaoRecusaEstadoImpossivel(t *testing.T) {
	id, p := ids(t)

	t.Run("saldo negativo persistido", func(t *testing.T) {
		negativo, err := brl(t, "1.00").Neg()
		require.NoError(t, err)
		_, err = wallet.Rehydrate(id, p, negativo, 1, agora, agora)
		assert.ErrorIs(t, err, wallet.ErrInvalidState,
			"saldo negativo no banco significa invariante violada; não se opera sobre ele")
	})

	t.Run("versão abaixo da inicial", func(t *testing.T) {
		_, err := wallet.Rehydrate(id, p, brl(t, "10.00"), 0, agora, agora)
		assert.ErrorIs(t, err, wallet.ErrInvalidState)
	})
}

// AC-4
func TestDebitoReduzSaldoEAvancaVersao(t *testing.T) {
	w := aberta(t, "100.00")

	mv, err := w.Debit(brl(t, "25.00"), agora)
	require.NoError(t, err)

	assert.Equal(t, "75.00", w.Balance().Amount())
	assert.Equal(t, int64(2), w.Version())
	assert.Equal(t, wallet.Debit, mv.Direction)
	assert.Equal(t, "100.00", mv.BalanceBefore.Amount())
	assert.Equal(t, "75.00", mv.BalanceAfter.Amount())
	assert.Equal(t, int64(2), mv.Version)
}

// AC-5: a recusa não pode deixar rastro. O caso de uso ainda vai gravar a
// transação como recusada usando o saldo atual.
func TestDebitoMaiorQueOSaldoNaoAlteraNada(t *testing.T) {
	w := aberta(t, "100.00")

	_, err := w.Debit(brl(t, "100.01"), agora)

	require.ErrorIs(t, err, wallet.ErrInsufficientFunds)
	assert.Equal(t, "100.00", w.Balance().Amount(), "o saldo não pode ter mudado")
	assert.Equal(t, int64(1), w.Version(), "a versão não pode ter avançado")
}

// AC-6: gastar tudo é permitido; o que é proibido é ficar negativo.
func TestDebitoExatamenteIgualAoSaldoEhPermitido(t *testing.T) {
	w := aberta(t, "100.00")

	_, err := w.Debit(brl(t, "100.00"), agora)
	require.NoError(t, err)

	assert.True(t, w.Balance().IsZero())
	assert.Equal(t, int64(2), w.Version())
}

func TestCreditoAumentaSaldo(t *testing.T) {
	w := aberta(t, "100.00")

	mv, err := w.Credit(brl(t, "50.00"), agora)
	require.NoError(t, err)

	assert.Equal(t, "150.00", w.Balance().Amount())
	assert.Equal(t, wallet.Credit, mv.Direction)
	assert.Equal(t, int64(2), w.Version())
}

// AC-7
func TestMovimentacaoEmMoedaDiferenteEhRecusada(t *testing.T) {
	w := aberta(t, "100.00")
	dolares, err := money.Parse("10.00", money.USD)
	require.NoError(t, err)

	_, err = w.Debit(dolares, agora)
	assert.ErrorIs(t, err, wallet.ErrCurrencyMismatch)

	_, err = w.Credit(dolares, agora)
	assert.ErrorIs(t, err, wallet.ErrCurrencyMismatch)
}

// AC-8
func TestMovimentacaoExigeValorPositivo(t *testing.T) {
	w := aberta(t, "100.00")
	negativo, err := brl(t, "10.00").Neg()
	require.NoError(t, err)

	for nome, valor := range map[string]money.Money{
		"zero":     brl(t, "0.00"),
		"negativo": negativo,
	} {
		t.Run(nome, func(t *testing.T) {
			_, err := w.Debit(valor, agora)
			assert.ErrorIs(t, err, wallet.ErrNonPositiveAmount)

			_, err = w.Credit(valor, agora)
			assert.ErrorIs(t, err, wallet.ErrNonPositiveAmount)
		})
	}
}

// AC-E1
func TestCarteiraNaoInicializadaEhRecusada(t *testing.T) {
	var vazia wallet.Wallet

	_, err := vazia.Debit(brl(t, "10.00"), agora)
	assert.ErrorIs(t, err, wallet.ErrUninitialized)

	_, err = vazia.Credit(brl(t, "10.00"), agora)
	assert.ErrorIs(t, err, wallet.ErrUninitialized)
}

func TestPertenceAoJogador(t *testing.T) {
	id, p := ids(t)
	w, _, err := wallet.Open(id, p, brl(t, "10.00"), agora)
	require.NoError(t, err)

	outro, err := wallet.NewPlayerID()
	require.NoError(t, err)

	assert.True(t, w.BelongsTo(p))
	assert.False(t, w.BelongsTo(outro))
}

// Uma sequência de movimentações mantém a coerência entre saldo, versão e os
// extremos de cada movimentação — que é o que o ledger vai registrar.
func TestSequenciaDeMovimentacoesMantemCoerencia(t *testing.T) {
	w := aberta(t, "100.00")

	d, err := w.Debit(brl(t, "30.00"), agora)
	require.NoError(t, err)
	c, err := w.Credit(brl(t, "5.00"), agora)
	require.NoError(t, err)

	assert.Equal(t, "75.00", w.Balance().Amount())
	assert.Equal(t, int64(3), w.Version())

	// O saldo posterior de uma é o anterior da seguinte: sem isso, a soma dos
	// lançamentos não reconstruiria o saldo.
	assert.Equal(t, d.BalanceAfter.Amount(), c.BalanceBefore.Amount())
	assert.Equal(t, w.Balance().Amount(), c.BalanceAfter.Amount())
}

func TestInstantesSaoNormalizadosParaUTC(t *testing.T) {
	saoPaulo := time.FixedZone("BRT", -3*60*60)
	id, p := ids(t)

	w, mv, err := wallet.Open(id, p, brl(t, "10.00"), agora.In(saoPaulo))
	require.NoError(t, err)

	assert.Equal(t, time.UTC, w.CreatedAt().Location())
	assert.Equal(t, time.UTC, mv.OccurredAt.Location())
}
