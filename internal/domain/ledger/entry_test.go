package ledger_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/internal/domain/ledger"
	"github.com/Lauiskk/munchkin/internal/domain/money"
	"github.com/Lauiskk/munchkin/internal/domain/wagering"
	"github.com/Lauiskk/munchkin/internal/domain/wallet"
)

var agora = time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

func brl(t *testing.T, v string) money.Money {
	t.Helper()
	m, err := money.Parse(v, money.BRL)
	require.NoError(t, err)
	return m
}

func chaves(t *testing.T) (ledger.ID, wallet.ID, wagering.TransactionID) {
	t.Helper()
	lid, err := ledger.NewID()
	require.NoError(t, err)
	wid, err := wallet.NewID()
	require.NoError(t, err)
	tid, err := wagering.NewTransactionID()
	require.NoError(t, err)
	return lid, wid, tid
}

// AC-11: a aritmética é a invariante central. É por causa dela que a
// reconciliação pode somar os lançamentos e confiar no resultado.
func TestAritmeticaDoLancamentoEhConferida(t *testing.T) {
	lid, wid, tid := chaves(t)

	t.Run("débito correto", func(t *testing.T) {
		e, err := ledger.New(lid, wid, tid, wallet.Debit,
			brl(t, "80.00"), brl(t, "100.00"), brl(t, "20.00"), agora)
		require.NoError(t, err)
		assert.Equal(t, "20.00", e.BalanceAfter().Amount())
	})

	t.Run("crédito correto", func(t *testing.T) {
		e, err := ledger.New(lid, wid, tid, wallet.Credit,
			brl(t, "50.00"), brl(t, "100.00"), brl(t, "150.00"), agora)
		require.NoError(t, err)
		assert.Equal(t, "150.00", e.BalanceAfter().Amount())
	})

	t.Run("débito que não fecha", func(t *testing.T) {
		_, err := ledger.New(lid, wid, tid, wallet.Debit,
			brl(t, "80.00"), brl(t, "100.00"), brl(t, "19.99"), agora)
		assert.ErrorIs(t, err, ledger.ErrBalanceMismatch)
	})

	t.Run("crédito lançado como débito", func(t *testing.T) {
		// Direção trocada: os números seriam de um crédito, a direção diz débito.
		_, err := ledger.New(lid, wid, tid, wallet.Debit,
			brl(t, "50.00"), brl(t, "100.00"), brl(t, "150.00"), agora)
		assert.ErrorIs(t, err, ledger.ErrBalanceMismatch)
	})
}

func TestLancamentoRecusaEntradaInvalida(t *testing.T) {
	lid, wid, tid := chaves(t)

	t.Run("valor zero", func(t *testing.T) {
		_, err := ledger.New(lid, wid, tid, wallet.Credit,
			brl(t, "0.00"), brl(t, "100.00"), brl(t, "100.00"), agora)
		assert.ErrorIs(t, err, ledger.ErrNonPositiveAmount)
	})

	t.Run("saldo posterior negativo", func(t *testing.T) {
		negativo, err := brl(t, "20.00").Neg()
		require.NoError(t, err)
		_, err = ledger.New(lid, wid, tid, wallet.Debit,
			brl(t, "120.00"), brl(t, "100.00"), negativo, agora)
		assert.ErrorIs(t, err, ledger.ErrNegativeBalance)
	})

	t.Run("moedas divergentes", func(t *testing.T) {
		dolares, err := money.Parse("80.00", money.USD)
		require.NoError(t, err)
		_, err = ledger.New(lid, wid, tid, wallet.Debit,
			dolares, brl(t, "100.00"), brl(t, "20.00"), agora)
		assert.ErrorIs(t, err, ledger.ErrCurrencyMismatch)
	})

	t.Run("direção desconhecida", func(t *testing.T) {
		_, err := ledger.New(lid, wid, tid, wallet.Direction("TRANSFER"),
			brl(t, "10.00"), brl(t, "100.00"), brl(t, "110.00"), agora)
		require.Error(t, err)
	})

	t.Run("identificador ausente", func(t *testing.T) {
		_, err := ledger.New(ledger.ID{}, wid, tid, wallet.Credit,
			brl(t, "10.00"), brl(t, "100.00"), brl(t, "110.00"), agora)
		assert.ErrorIs(t, err, ledger.ErrInvalidID)
	})
}

// Construir a partir da movimentação do agregado é o caminho normal: elimina a
// chance de o ledger discordar da carteira por recálculo próprio.
func TestLancamentoAPartirDaMovimentacaoDaCarteira(t *testing.T) {
	lid, _, tid := chaves(t)
	wid, err := wallet.NewID()
	require.NoError(t, err)
	pid, err := wallet.NewPlayerID()
	require.NoError(t, err)

	w, _, err := wallet.Open(wid, pid, brl(t, "100.00"), agora)
	require.NoError(t, err)
	mv, err := w.Debit(brl(t, "80.00"), agora)
	require.NoError(t, err)

	e, err := ledger.FromMovement(lid, w.ID(), tid, mv, agora)
	require.NoError(t, err)

	assert.Equal(t, wallet.Debit, e.Direction())
	assert.Equal(t, "80.00", e.Amount().Amount())
	assert.Equal(t, "100.00", e.BalanceBefore().Amount())
	assert.Equal(t, "20.00", e.BalanceAfter().Amount())
	assert.Equal(t, w.Balance().Amount(), e.BalanceAfter().Amount(),
		"o lançamento tem de terminar onde a carteira está")
}

// A reconciliação reconstrói o saldo somando os lançamentos com sinal. Este
// teste é a prova em miniatura de que isso funciona.
func TestSomaDosLancamentosReconstroiOSaldo(t *testing.T) {
	wid, err := wallet.NewID()
	require.NoError(t, err)
	pid, err := wallet.NewPlayerID()
	require.NoError(t, err)

	w, abertura, err := wallet.Open(wid, pid, brl(t, "1000.00"), agora)
	require.NoError(t, err)

	movimentos := []wallet.Movement{*abertura}
	for _, op := range []struct {
		debito bool
		valor  string
	}{
		{true, "80.00"}, {false, "150.00"}, {true, "25.50"}, {true, "0.01"},
	} {
		var mv wallet.Movement
		if op.debito {
			mv, err = w.Debit(brl(t, op.valor), agora)
		} else {
			mv, err = w.Credit(brl(t, op.valor), agora)
		}
		require.NoError(t, err)
		movimentos = append(movimentos, mv)
	}

	reconstruido := brl(t, "0.00")
	for i, mv := range movimentos {
		lid, err := ledger.NewID()
		require.NoError(t, err)
		tid, err := wagering.NewTransactionID()
		require.NoError(t, err)

		e, err := ledger.FromMovement(lid, w.ID(), tid, mv, agora)
		require.NoError(t, err, "lançamento %d", i)

		comSinal, err := e.SignedAmount()
		require.NoError(t, err)
		reconstruido, err = reconstruido.Add(comSinal)
		require.NoError(t, err)
	}

	assert.Equal(t, w.Balance().Amount(), reconstruido.Amount(),
		"a soma dos lançamentos precisa dar o saldo armazenado")
	assert.Equal(t, "1044.49", reconstruido.Amount())
}
