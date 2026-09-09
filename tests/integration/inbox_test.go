//go:build integration

package integration_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/internal/adapter/postgres"
	"github.com/Lauiskk/munchkin/internal/app/inbox"
	appwagering "github.com/Lauiskk/munchkin/internal/app/wagering"
	domain "github.com/Lauiskk/munchkin/internal/domain/wagering"
)

const consumidorDeTeste = "teste"

type ambienteInbox struct {
	ambienteOperacao
	handler *inbox.Handler
}

func novoAmbienteInbox(t *testing.T) ambienteInbox {
	t.Helper()
	a := novoAmbienteOperacao(t)
	return ambienteInbox{
		ambienteOperacao: a,
		handler: inbox.NewHandler(a.db, postgres.NewInboxRepository(a.db), a.processor,
			slog.New(slog.NewTextHandler(io.Discard, nil)), consumidorDeTeste),
	}
}

// mensagem monta o que o transporte entregaria já validado.
func mensagem(id string, hash string, in appwagering.Input) inbox.Message {
	return inbox.Message{ID: id, Hash: []byte(hash), Input: in}
}

// AC-1
func TestMensagemValidaProcessaEConcluiNaInbox(t *testing.T) {
	a := novoAmbienteInbox(t)
	w, p := a.carteira(t, "100.00")

	desfecho, saida, err := a.handler.Handle(a.ctx,
		mensagem("msg-1", "h1", operacao(t, w, p, "tx-1", domain.Bet, "30.00")))

	require.NoError(t, err)
	assert.Equal(t, inbox.Processed, desfecho)
	assert.Equal(t, domain.Processed, saida.Status)
	assert.Equal(t, "70.00 BRL", a.saldo(t, w))
	assert.Equal(t, int64(1), a.conta(t, `SELECT count(*) FROM inbox_messages
		WHERE consumer_name = ? AND message_id = 'msg-1' AND completed_at IS NOT NULL`,
		consumidorDeTeste), "a conclusão é o que autoriza remover a mensagem da fila")
}

// AC-2 — o registro da mensagem e o efeito de domínio são indivisíveis.
//
// Se a inbox pudesse ser gravada sem o efeito, a reentrega — que é a única
// chance de corrigir — seria descartada como duplicata, e o dinheiro nunca se
// moveria. É o pior desfecho possível: silencioso e irreversível.
func TestInboxEDominioCommitamJuntosOuNaoCommitam(t *testing.T) {
	a := novoAmbienteInbox(t)
	w, p := a.carteira(t, "100.00")

	quebrado := appwagering.NewProcessor(a.db,
		postgres.NewWalletRepository(a.db), postgres.NewTransactionRepository(a.db),
		postgres.NewLedgerRepository(a.db),
		outboxQueFalha{err: errors.New("outbox indisponível")},
		relogioFixo{t: agoraFixo})
	handler := inbox.NewHandler(a.db, postgres.NewInboxRepository(a.db), quebrado,
		slog.New(slog.NewTextHandler(io.Discard, nil)), consumidorDeTeste)

	_, _, err := handler.Handle(a.ctx,
		mensagem("msg-1", "h1", operacao(t, w, p, "tx-1", domain.Bet, "30.00")))

	require.Error(t, err)
	assert.Equal(t, "100.00 BRL", a.saldo(t, w), "o saldo não pode ter mudado")
	assert.Zero(t, a.conta(t, `SELECT count(*) FROM inbox_messages WHERE message_id = 'msg-1'`),
		"a mensagem não pode constar como recebida se nada foi gravado")
	assert.Zero(t, a.conta(t, `SELECT count(*) FROM wager_transactions
		WHERE external_transaction_id = 'tx-1'`))
}

// AC-3
func TestReentregaDeMensagemConcluidaNaoReprocessa(t *testing.T) {
	a := novoAmbienteInbox(t)
	w, p := a.carteira(t, "100.00")
	msg := mensagem("msg-1", "h1", operacao(t, w, p, "tx-1", domain.Bet, "30.00"))

	_, _, err := a.handler.Handle(a.ctx, msg)
	require.NoError(t, err)

	desfecho, _, err := a.handler.Handle(a.ctx, msg)

	require.NoError(t, err)
	assert.Equal(t, inbox.Duplicate, desfecho)
	assert.Equal(t, "70.00 BRL", a.saldo(t, w), "um único débito, não dois")
	assert.Equal(t, int64(2), a.conta(t, `SELECT count(*) FROM wallet_ledger_entries
		WHERE wallet_id = ?`, uuid.UUID(w)), "abertura e aposta; a reentrega não lança")
}

// AC-4
func TestMesmaIdentidadeComConteudoDiferenteEhInvalida(t *testing.T) {
	a := novoAmbienteInbox(t)
	w, p := a.carteira(t, "100.00")

	_, _, err := a.handler.Handle(a.ctx,
		mensagem("msg-1", "h1", operacao(t, w, p, "tx-1", domain.Bet, "30.00")))
	require.NoError(t, err)

	desfecho, _, err := a.handler.Handle(a.ctx,
		mensagem("msg-1", "OUTRO-HASH", operacao(t, w, p, "tx-2", domain.Bet, "10.00")))

	require.NoError(t, err, "mensagem inválida é desfecho, não erro de infraestrutura")
	assert.Equal(t, inbox.HashMismatch, desfecho)
	assert.Equal(t, "70.00 BRL", a.saldo(t, w),
		"aceitar o conteúdo novo deixaria alguém reescrever o passado")
	assert.Zero(t, a.conta(t, `SELECT count(*) FROM wager_transactions
		WHERE external_transaction_id = 'tx-2'`))
}

// AC-5 — recusa de negócio confirmada é terminal.
func TestRecusaDeNegocioConcluiAMensagem(t *testing.T) {
	a := novoAmbienteInbox(t)
	w, p := a.carteira(t, "10.00")

	desfecho, saida, err := a.handler.Handle(a.ctx,
		mensagem("msg-1", "h1", operacao(t, w, p, "tx-1", domain.Bet, "500.00")))

	require.NoError(t, err)
	assert.Equal(t, inbox.Processed, desfecho,
		"retentar produziria a mesma recusa para sempre: a mensagem sai da fila")
	assert.Equal(t, domain.Rejected, saida.Status)
	assert.Equal(t, domain.FailureInsufficientFunds, saida.FailureCode)
	assert.Equal(t, int64(1), a.conta(t, `SELECT count(*) FROM inbox_messages
		WHERE message_id = 'msg-1' AND completed_at IS NOT NULL`))
	assert.Equal(t, int64(1), a.conta(t, `SELECT count(*) FROM outbox_events
		WHERE aggregate_id = ? AND event_type = 'WagerTransactionRejected'`, uuid.UUID(w)),
		"a recusa é um fato publicado, não um silêncio")
}

// AC-6 — um tratamento que morre antes do commit deixa a mensagem retentável.
func TestTratamentoInterrompidoDeixaAMensagemRetentavel(t *testing.T) {
	a := novoAmbienteInbox(t)
	w, p := a.carteira(t, "100.00")

	quebrado := inbox.NewHandler(a.db, repoInboxQueFalhaAoConcluir{
		Repository: postgres.NewInboxRepository(a.db),
	}, a.processor, slog.New(slog.NewTextHandler(io.Discard, nil)), consumidorDeTeste)

	msg := mensagem("msg-1", "h1", operacao(t, w, p, "tx-1", domain.Bet, "30.00"))
	_, _, err := quebrado.Handle(a.ctx, msg)
	require.Error(t, err)
	assert.Equal(t, "100.00 BRL", a.saldo(t, w), "nada commitou")

	// A reentrega encontra tudo limpo e trata do zero.
	desfecho, _, err := a.handler.Handle(a.ctx, msg)

	require.NoError(t, err)
	assert.Equal(t, inbox.Processed, desfecho)
	assert.Equal(t, "70.00 BRL", a.saldo(t, w))
}

// repoInboxQueFalhaAoConcluir simula a queda depois do efeito de domínio e
// antes de a mensagem ser dada como concluída.
type repoInboxQueFalhaAoConcluir struct{ inbox.Repository }

func (repoInboxQueFalhaAoConcluir) Complete(context.Context, string, string) error {
	return errors.New("processo caiu antes de concluir")
}
