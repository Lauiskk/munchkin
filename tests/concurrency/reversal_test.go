//go:build integration

package concurrency_test

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/internal/adapter/postgres"
	appwagering "github.com/Lauiskk/munchkin/internal/app/wagering"
	domain "github.com/Lauiskk/munchkin/internal/domain/wagering"
	domainwallet "github.com/Lauiskk/munchkin/internal/domain/wallet"
)

// relogioAdiantado enxerga o futuro. As pendências nascem agendadas para daqui
// a alguns segundos, e dormir por elas só tornaria a suíte mais lenta sem
// tornar a corrida mais real — o que se quer observar é a disputa entre dois
// resolvedores, não a passagem do tempo.
type relogioAdiantado struct{ d time.Duration }

func (r relogioAdiantado) Now() time.Time { return time.Now().UTC().Add(r.d) }

// novoResolver monta um resolvedor independente sobre o mesmo banco.
//
// Duas instâncias da aplicação diferem no processo, não no banco. A
// verificação com processos realmente separados, cada um com seu binário e seu
// pool, é do CP-16; aqui o que se exercita é a coordenação no banco.
func (c cenario) novoResolver(saida io.Writer) *appwagering.Resolver {
	return appwagering.NewResolver(c.db,
		postgres.NewTransactionRepository(c.db),
		postgres.NewWalletRepository(c.db),
		c.processor,
		slog.New(slog.NewJSONHandler(saida, nil)),
		relogioAdiantado{d: time.Minute})
}

// diario acumula o log dos workers para que o teste possa cobrá-lo.
//
// Vários resolvedores escrevem ao mesmo tempo; sem o mutex o próprio teste
// teria uma corrida, e o -race apontaria para o teste em vez de para o código.
type diario struct {
	mu    sync.Mutex
	texto strings.Builder
}

func (d *diario) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.texto.Write(p)
}

func (d *diario) String() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.texto.String()
}

// estorno monta a entrada de uma reversão apontando para outra operação.
func estorno(t *testing.T, w domainwallet.ID, p domainwallet.PlayerID,
	externo, valor, referencia string) appwagering.Input {
	t.Helper()
	in := aposta(t, w, p, externo, valor)
	in.Kind = domain.Refund
	in.ReferenceExternalID = domain.ExternalID(referencia)
	return in
}

// AC-12 — workers concorrentes varrendo a mesma fila não podem processar a
// mesma pendência duas vezes. Se processassem, o jogador receberia o estorno em
// dobro: o pior desfecho possível num sistema que move dinheiro.
//
// As pendências ficam em carteiras distintas de propósito. Com uma carteira só,
// o lock da carteira serializaria tudo e a disputa pelas pendências nunca
// aconteceria — o teste passaria sem exercitar nada.
//
// A garantia é sustentada por camadas independentes — SKIP LOCKED, o predicado
// `status = 'PENDING_REFERENCE'` no lock, a recusa por
// REFERENCE_ALREADY_REVERSED, a imutabilidade dos estados terminais e o índice
// único do ledger. Cobrar só o saldo final não distingue nenhuma delas: por
// mutação, removendo as duas primeiras o teste continuava verde, porque as
// demais seguravam.
//
// Por isso a asserção que importa aqui não é apenas o resultado, é o CAMINHO:
// quem perde a disputa tem de PULAR a pendência, em silêncio. Se ela chegar a
// ser tentada e falhar mais adiante, o log acusa — e é isso que se cobra.
func TestDoisResolvedoresNaoProcessamAMesmaPendencia(t *testing.T) {
	const (
		pendencias   = 20
		concorrentes = 4
	)

	c := novoCenario(t)

	carteiras := make([]domainwallet.ID, pendencias)
	reversoes := make([]domain.TransactionID, pendencias)

	for i := range pendencias {
		w, p := c.abrirCarteira(t, "100.00")
		carteiras[i] = w

		// O estorno chega antes da aposta: fica PENDING_REFERENCE, aguardando.
		out, err := c.processor.Process(c.ctx,
			estorno(t, w, p, fmt.Sprintf("rf-%d", i), "80.00", fmt.Sprintf("bet-%d", i)))
		require.NoError(t, err)
		require.Equal(t, domain.PendingReference, out.Status)
		reversoes[i] = out.TransactionID

		// Agora a referência existe, e a pendência passa a ser resolvível.
		apostaOut, err := c.processor.Process(c.ctx,
			aposta(t, w, p, fmt.Sprintf("bet-%d", i), "80.00"))
		require.NoError(t, err)
		require.Equal(t, domain.Processed, apostaOut.Status)
	}

	var log diario
	workers := make([]*appwagering.Resolver, concorrentes)
	for i := range workers {
		workers[i] = c.novoResolver(&log)
	}
	tratadas := make([]int, concorrentes)
	erros := make([]error, concorrentes)

	// A largada é simultânea de propósito: sem a barreira, o primeiro worker
	// terminaria a fila antes do segundo chegar a consultá-la, e a disputa que
	// se quer observar simplesmente não aconteceria.
	largada := make(chan struct{})
	var wg sync.WaitGroup
	for i, w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-largada
			tratadas[i], erros[i] = w.RunOnce(c.ctx)
		}()
	}
	close(largada)
	wg.Wait()

	total := 0
	for i := range concorrentes {
		require.NoError(t, erros[i])
		total += tratadas[i]
	}

	assert.Equal(t, pendencias, total,
		"cada pendência tratada exatamente uma vez pelo conjunto dos workers")
	assert.NotContains(t, log.String(), "resolver.failed",
		"perder a disputa é rotina e tem de sair limpo; erro aqui significa que "+
			"o worker chegou a processar algo que já era de outro")

	for i, id := range reversoes {
		assert.Equal(t, int64(1), c.escalar(t, `SELECT count(*) FROM wager_transactions
			WHERE id = ? AND status = 'PROCESSED'`, id.String()),
			"reversão %d não concluiu", i)
		assert.Equal(t, int64(1), c.escalar(t, `SELECT count(*) FROM wallet_ledger_entries
			WHERE transaction_id = ?`, id.String()),
			"reversão %d gerou lançamento duplicado", i)
		assert.Equal(t, int64(10000), c.escalar(t,
			"SELECT balance_minor FROM wallets WHERE id = ?", carteiras[i].String()),
			"carteira %d: 100.00 menos a aposta de 80.00, mais o estorno de 80.00", i)
	}

	assert.Zero(t, c.escalar(t, `SELECT count(*) FROM wager_transactions
		WHERE status = 'PENDING_REFERENCE'`), "nenhuma pendência pode sobrar")
}
