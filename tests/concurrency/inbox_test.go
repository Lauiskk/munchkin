//go:build integration

package concurrency_test

import (
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/internal/adapter/postgres"
	"github.com/Lauiskk/munchkin/internal/app/inbox"
	appwagering "github.com/Lauiskk/munchkin/internal/app/wagering"
)

// AC-8 — a MESMA operação chegando por HTTP e por mensageria ao mesmo tempo.
//
// É o cenário que o §10 manda validar, e o mais fácil de errar: cada transporte
// tem a sua deduplicação — a inbox de um lado, a chave de idempotência do outro
// — e é tentador achar que cada uma resolve o seu lado. Não resolvem. A inbox
// não sabe da requisição HTTP, e a requisição HTTP não sabe da mensagem. Quem
// decide é o índice único de (provider, externalTransactionId) no banco, que os
// dois caminhos atravessam.
func TestMesmaOperacaoPorHTTPEPorFilaProduzUmDebitoSo(t *testing.T) {
	const (
		porHTTP = 5
		porFila = 5
	)

	c := novoCenario(t)
	w, p := c.abrirCarteira(t, "100.00")

	handler := inbox.NewHandler(c.db, postgres.NewInboxRepository(c.db), c.processor,
		slog.New(slog.NewJSONHandler(io.Discard, nil)), inbox.ConsumerWagerTransactions)

	// Mesma chave de idempotência, mesmo identificador externo: para o negócio,
	// é uma operação só, tenha ela vindo por onde vier.
	operacao := aposta(t, w, p, "tx-1", "30.00")

	largada := make(chan struct{})
	saidas := make([]appwagering.Output, porHTTP+porFila)
	erros := make([]error, porHTTP+porFila)

	var wg sync.WaitGroup
	for i := range porHTTP + porFila {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-largada
			if i < porHTTP {
				saidas[i], erros[i] = c.processor.Process(c.ctx, operacao)
				return
			}
			// Cada mensagem tem identidade própria — são entregas distintas da
			// mesma operação, não reentregas da mesma mensagem.
			_, saidas[i], erros[i] = handler.Handle(c.ctx, inbox.Message{
				ID:    fmt.Sprintf("msg-%d", i),
				Hash:  []byte(fmt.Sprintf("hash-%d", i)),
				Input: operacao,
			})
		}()
	}
	close(largada)
	wg.Wait()

	replays := 0
	for i := range saidas {
		require.NoError(t, erros[i])
		if saidas[i].IdempotentReplay {
			replays++
		}
	}

	assert.Equal(t, porHTTP+porFila-1, replays,
		"exatamente um caminho processa; todos os outros enxergam o replay")
	assert.Equal(t, int64(7000),
		c.escalar(t, "SELECT balance_minor FROM wallets WHERE id = ?", w.String()),
		"saldo final de 70.00: um único débito de 30.00")
	assert.Equal(t, int64(1),
		c.escalar(t, `SELECT count(*) FROM wallet_ledger_entries
		               WHERE wallet_id = ? AND direction = 'DEBIT'`, w.String()),
		"um único lançamento de débito")
	assert.Equal(t, int64(1),
		c.escalar(t, `SELECT count(*) FROM wager_transactions
		               WHERE provider_id = 'provider-a' AND external_transaction_id = 'tx-1'`),
		"uma única transação, não uma por transporte")
	assert.Equal(t, int64(porFila),
		c.escalar(t, `SELECT count(*) FROM inbox_messages WHERE completed_at IS NOT NULL`),
		"cada mensagem é concluída: elas são entregas distintas, e todas foram tratadas")
}
