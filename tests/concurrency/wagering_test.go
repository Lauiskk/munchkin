//go:build integration

package concurrency_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	appwagering "github.com/Lauiskk/munchkin/internal/app/wagering"
	"github.com/Lauiskk/munchkin/internal/domain/money"
	domain "github.com/Lauiskk/munchkin/internal/domain/wagering"
	domainwallet "github.com/Lauiskk/munchkin/internal/domain/wallet"
)

func aposta(t *testing.T, w domainwallet.ID, p domainwallet.PlayerID, externo, valor string) appwagering.Input {
	t.Helper()
	m, err := money.Parse(valor, money.BRL)
	require.NoError(t, err)
	return appwagering.Input{
		ProviderID:     "provider-a",
		ExternalID:     domain.ExternalID(externo),
		IdempotencyKey: "provider-a:" + externo,
		PlayerID:       p,
		WalletID:       w,
		RoundID:        "round-1",
		GameID:         "fortune-chimp",
		Kind:           domain.Bet,
		Money:          m,
	}
}

// resultado guarda o desfecho de uma execução concorrente.
type resultado struct {
	out appwagering.Output
	err error
}

// emParalelo dispara n execuções ao mesmo tempo, liberadas por uma barreira.
//
// A barreira importa: sem ela, as goroutines começam escalonadas e o teste
// mediria a ordem do escalonador, não a contenção.
func emParalelo(n int, fn func(i int) resultado) []resultado {
	saida := make([]resultado, n)
	largada := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-largada
			saida[i] = fn(i)
		}(i)
	}
	close(largada)
	wg.Wait()
	return saida
}

// O cenário que o §8 do enunciado fixa, e que define a nota: uma carteira com
// 100.00 recebe, ao mesmo tempo, duas apostas distintas de 80.00.
func TestDuasApostasDeOitentaSobreCemReais(t *testing.T) {
	c := novoCenario(t)
	w, p := c.abrirCarteira(t, "100.00")

	res := emParalelo(2, func(i int) resultado {
		externo := fmt.Sprintf("bet-%d", i)
		out, err := c.processor.Process(c.ctx, aposta(t, w, p, externo, "80.00"))
		return resultado{out: out, err: err}
	})

	var processadas, recusadas int
	for _, r := range res {
		require.NoError(t, r.err)
		switch r.out.Status {
		case domain.Processed:
			processadas++
		case domain.Rejected:
			recusadas++
			assert.Equal(t, domain.FailureInsufficientFunds, r.out.FailureCode,
				"a recusa tem de ser por saldo, com código próprio")
		}
	}

	assert.Equal(t, 1, processadas, "exatamente uma aposta processada")
	assert.Equal(t, 1, recusadas, "exatamente uma recusada")

	assert.Equal(t, int64(2000),
		c.escalar(t, "SELECT balance_minor FROM wallets WHERE id = ?", w.String()),
		"saldo final de 20.00")

	assert.Equal(t, int64(1),
		c.escalar(t, `SELECT count(*) FROM wallet_ledger_entries
		               WHERE wallet_id = ? AND direction = 'DEBIT'`, w.String()),
		"um único débito no ledger")

	assert.Equal(t, int64(2),
		c.escalar(t, "SELECT version FROM wallets WHERE id = ?", w.String()),
		"a versão avança uma vez: só uma mudança de saldo aconteceu")
}

// §13.1: a mesma aposta enviada cinquenta vezes em paralelo produz um único
// débito. É a prova de que a idempotência aguenta concorrência — e não apenas
// reenvios sequenciais, que qualquer cache resolveria.
func TestCinquentaEnviosParalelosDaMesmaAposta(t *testing.T) {
	const envios = 50

	c := novoCenario(t)
	w, p := c.abrirCarteira(t, "1000.00")

	res := emParalelo(envios, func(int) resultado {
		out, err := c.processor.Process(c.ctx, aposta(t, w, p, "same-bet", "25.00"))
		return resultado{out: out, err: err}
	})

	var originais, replays int
	saldos := map[string]int{}
	for _, r := range res {
		require.NoError(t, r.err)
		require.Equal(t, domain.Processed, r.out.Status)
		if r.out.IdempotentReplay {
			replays++
		} else {
			originais++
		}
		saldos[r.out.Balance.Amount()]++
	}

	assert.Equal(t, 1, originais, "apenas um envio aplica a operação")
	assert.Equal(t, envios-1, replays, "os demais são replays")
	assert.Len(t, saldos, 1, "todos devolvem o MESMO saldo: %v", saldos)
	assert.Contains(t, saldos, "975.00")

	assert.Equal(t, int64(1),
		c.escalar(t, `SELECT count(*) FROM wallet_ledger_entries
		               WHERE wallet_id = ? AND direction = 'DEBIT'`, w.String()),
		"um único débito, apesar dos cinquenta envios")
	assert.Equal(t, int64(1),
		c.escalar(t, `SELECT count(*) FROM wager_transactions
		               WHERE wallet_id = ? AND external_transaction_id = 'same-bet'`, w.String()))
	assert.Equal(t, int64(97500),
		c.escalar(t, "SELECT balance_minor FROM wallets WHERE id = ?", w.String()))
}

// §13.3: carteiras distintas avançam em paralelo. Se o lock fosse global — ou de
// tabela —, este teste passaria mesmo assim, mas a serialização apareceria como
// tempo; o que ele garante é a CORREÇÃO de cada uma sob concorrência cruzada.
func TestCarteirasDistintasNaoInterferemEntreSi(t *testing.T) {
	const carteiras = 8
	const apostasPorCarteira = 5

	c := novoCenario(t)

	ids := make([]domainwallet.ID, carteiras)
	jogadores := make([]domainwallet.PlayerID, carteiras)
	for i := range ids {
		ids[i], jogadores[i] = c.abrirCarteira(t, "100.00")
	}

	res := emParalelo(carteiras*apostasPorCarteira, func(i int) resultado {
		carteira := i % carteiras
		seq := i / carteiras
		externo := fmt.Sprintf("w%d-bet%d", carteira, seq)
		out, err := c.processor.Process(c.ctx,
			aposta(t, ids[carteira], jogadores[carteira], externo, "10.00"))
		return resultado{out: out, err: err}
	})

	for _, r := range res {
		require.NoError(t, r.err)
		require.Equal(t, domain.Processed, r.out.Status)
	}

	// Cada carteira sofreu exatamente as suas cinco apostas de 10.00.
	for i, id := range ids {
		assert.Equal(t, int64(5000),
			c.escalar(t, "SELECT balance_minor FROM wallets WHERE id = ?", id.String()),
			"carteira %d deveria ter 50.00", i)
		assert.Equal(t, int64(apostasPorCarteira),
			c.escalar(t, `SELECT count(*) FROM wallet_ledger_entries
			               WHERE wallet_id = ? AND direction = 'DEBIT'`, id.String()),
			"carteira %d deveria ter exatamente %d débitos", i, apostasPorCarteira)
	}
}

// A soma dos lançamentos tem de reconstruir o saldo de cada carteira depois de
// toda a movimentação concorrente. É a conferência final que o §13 pede.
func TestSaldoBateComOLedgerDepoisDaConcorrencia(t *testing.T) {
	c := novoCenario(t)
	w, p := c.abrirCarteira(t, "500.00")

	res := emParalelo(20, func(i int) resultado {
		in := aposta(t, w, p, fmt.Sprintf("mix-%d", i), "10.00")
		if i%2 == 0 {
			in.Kind = domain.Win
		}
		out, err := c.processor.Process(c.ctx, in)
		return resultado{out: out, err: err}
	})
	for _, r := range res {
		require.NoError(t, r.err)
		require.Equal(t, domain.Processed, r.out.Status)
	}

	saldo := c.escalar(t, "SELECT balance_minor FROM wallets WHERE id = ?", w.String())
	somaLedger := c.escalar(t, `
		SELECT COALESCE(SUM(CASE WHEN direction = 'CREDIT' THEN amount_minor
		                         ELSE -amount_minor END), 0)
		  FROM wallet_ledger_entries WHERE wallet_id = ?`, w.String())

	assert.Equal(t, saldo, somaLedger,
		"o saldo armazenado tem de ser igual a créditos menos débitos")
	assert.Equal(t, int64(50000), saldo, "10 ganhos e 10 apostas de 10.00 se anulam")
}
