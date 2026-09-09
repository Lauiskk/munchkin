//go:build integration

package integration_test

import (
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/internal/adapter/postgres"
	appwagering "github.com/Lauiskk/munchkin/internal/app/wagering"
	domain "github.com/Lauiskk/munchkin/internal/domain/wagering"
	domainwallet "github.com/Lauiskk/munchkin/internal/domain/wallet"
)

// relogioMovel deixa o teste andar no tempo sem dormir. O backoff das
// pendências é medido em segundos e minutos; esperar de verdade transformaria
// a suíte inteira numa espera de dezenas de minutos.
type relogioMovel struct {
	mu    sync.Mutex
	agora time.Time
}

func (r *relogioMovel) Now() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.agora
}

func (r *relogioMovel) avancar(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.agora = r.agora.Add(d)
}

type ambienteReversao struct {
	ambienteOperacao
	resolver *appwagering.Resolver
	relogio  *relogioMovel
}

func novoAmbienteReversao(t *testing.T) ambienteReversao {
	t.Helper()
	a := novoAmbiente(t)
	rel := &relogioMovel{agora: agoraFixo}
	transacoes := postgres.NewTransactionRepository(a.db)
	carteiras := postgres.NewWalletRepository(a.db)
	proc := appwagering.NewProcessor(a.db, carteiras, transacoes,
		postgres.NewLedgerRepository(a.db), postgres.NewOutboxRepository(a.db), rel)

	return ambienteReversao{
		ambienteOperacao: ambienteOperacao{
			ambiente:  a,
			processor: proc,
			querier:   appwagering.NewQuerier(transacoes),
		},
		resolver: appwagering.NewResolver(a.db, transacoes, carteiras, proc,
			slog.New(slog.NewTextHandler(io.Discard, nil)), rel),
		relogio: rel,
	}
}

// reversao monta a entrada de um estorno apontando para outra operação externa.
func reversao(t *testing.T, w domainwallet.ID, p domainwallet.PlayerID,
	externo string, kind domain.Kind, montante, referencia string) appwagering.Input {
	t.Helper()
	in := operacao(t, w, p, externo, kind, montante)
	in.ReferenceExternalID = domain.ExternalID(referencia)
	return in
}

// processada roda uma operação e exige que ela tenha concluído.
func (a ambienteReversao) processada(t *testing.T, in appwagering.Input) appwagering.Output {
	t.Helper()
	out, err := a.processor.Process(a.ctx, in)
	require.NoError(t, err)
	require.Equal(t, domain.Processed, out.Status, "falha: %s", out.FailureCode)
	return out
}

// AC-1
func TestRefundDeApostaCreditaODebitoDeVolta(t *testing.T) {
	a := novoAmbienteReversao(t)
	w, p := a.carteira(t, "100.00")

	aposta := a.processada(t, operacao(t, w, p, "bet-1", domain.Bet, "80.00"))
	require.Equal(t, "20.00 BRL", aposta.Balance.String())

	estorno := a.processada(t, reversao(t, w, p, "rf-1", domain.Refund, "80.00", "bet-1"))

	assert.Equal(t, "100.00 BRL", estorno.Balance.String(),
		"o estorno tem que devolver exatamente o valor debitado")
	assert.Equal(t, int64(1), a.conta(t, `SELECT count(*) FROM wager_transactions
		WHERE id = ? AND reference_transaction_id = ?`,
		uuid.UUID(estorno.TransactionID), uuid.UUID(aposta.TransactionID)),
		"a reversão precisa apontar para a transação que reverteu")
	assert.Equal(t, int64(1), a.conta(t, `SELECT count(*) FROM wallet_ledger_entries
		WHERE transaction_id = ? AND direction = 'CREDIT'`, uuid.UUID(estorno.TransactionID)))
	assert.Equal(t, int64(1), a.conta(t, `SELECT count(*) FROM outbox_events
		WHERE aggregate_id = ? AND event_type = 'WalletBalanceChanged'
		  AND payload->>'transactionId' = ?`,
		uuid.UUID(w), estorno.TransactionID.String()))
}

// AC-2
func TestRollbackDeGanhoDebitaOMesmoValor(t *testing.T) {
	a := novoAmbienteReversao(t)
	w, p := a.carteira(t, "100.00")

	a.processada(t, operacao(t, w, p, "win-1", domain.Win, "50.00"))
	estorno := a.processada(t, reversao(t, w, p, "rb-1", domain.Rollback, "50.00", "win-1"))

	assert.Equal(t, "100.00 BRL", estorno.Balance.String())
	assert.Equal(t, int64(1), a.conta(t, `SELECT count(*) FROM wallet_ledger_entries
		WHERE transaction_id = ? AND direction = 'DEBIT'`, uuid.UUID(estorno.TransactionID)),
		"reverter um crédito tem que gerar um débito, não apagar o crédito")
}

// AC-3 — regressão: antes desta guarda o segundo estorno batia no índice único
// wager_transactions_single_reversal_uk e o cliente recebia erro interno.
func TestSegundaReversaoDaMesmaReferenciaERecusada(t *testing.T) {
	a := novoAmbienteReversao(t)
	w, p := a.carteira(t, "100.00")

	a.processada(t, operacao(t, w, p, "bet-1", domain.Bet, "40.00"))
	a.processada(t, reversao(t, w, p, "rf-1", domain.Refund, "40.00", "bet-1"))

	out, err := a.processor.Process(a.ctx,
		reversao(t, w, p, "rb-1", domain.Rollback, "40.00", "bet-1"))

	require.NoError(t, err, "a recusa é resposta de negócio, não erro de infraestrutura")
	assert.Equal(t, domain.Rejected, out.Status)
	assert.Equal(t, domain.FailureReferenceAlreadyReversed, out.FailureCode)
	assert.Equal(t, "100.00 BRL", out.Balance.String(), "o saldo não pode receber o crédito duas vezes")
	assert.Equal(t, int64(3), a.conta(t, `SELECT count(*) FROM wallet_ledger_entries
		WHERE wallet_id = ?`, uuid.UUID(w)),
		"abertura, aposta e o primeiro estorno lançam; o segundo não pode lançar nada")
}

// AC-4
func TestReversaoSemReferenciaFicaPendente(t *testing.T) {
	a := novoAmbienteReversao(t)
	w, p := a.carteira(t, "100.00")

	out, err := a.processor.Process(a.ctx,
		reversao(t, w, p, "rf-1", domain.Refund, "40.00", "ainda-nao-chegou"))

	require.NoError(t, err)
	assert.Equal(t, domain.PendingReference, out.Status)
	assert.Equal(t, "100.00 BRL", out.Balance.String(), "pendência não movimenta saldo")
	assert.Equal(t, int64(1), a.conta(t, `SELECT count(*) FROM outbox_events
		WHERE aggregate_id = ? AND event_type = 'WagerTransactionPendingReference'`, uuid.UUID(w)))
	assert.Equal(t, int64(1), a.conta(t, `SELECT count(*) FROM wager_transactions
		WHERE id = ? AND next_attempt_at IS NOT NULL`, uuid.UUID(out.TransactionID)),
		"a pendência precisa ficar agendada, senão nenhum worker a retoma")
}

// AC-5
func TestWorkerConcluiAPendenciaQuandoAReferenciaChega(t *testing.T) {
	a := novoAmbienteReversao(t)
	w, p := a.carteira(t, "100.00")

	pendente, err := a.processor.Process(a.ctx,
		reversao(t, w, p, "rf-1", domain.Refund, "80.00", "bet-1"))
	require.NoError(t, err)
	require.Equal(t, domain.PendingReference, pendente.Status)

	// A aposta chega depois do estorno — a ordem de entrega não é garantida.
	a.processada(t, operacao(t, w, p, "bet-1", domain.Bet, "80.00"))

	a.relogio.avancar(appwagering.Backoff(1))
	tratadas, err := a.resolver.RunOnce(a.ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, tratadas)

	assert.Equal(t, int64(1), a.conta(t, `SELECT count(*) FROM wager_transactions
		WHERE id = ? AND status = 'PROCESSED'`, uuid.UUID(pendente.TransactionID)))
	assert.Equal(t, int64(1), a.conta(t, `SELECT count(*) FROM wallets
		WHERE id = ? AND balance_minor = 10000`, uuid.UUID(w)),
		"debitou 80 na aposta e devolveu 80 no estorno")
	assert.Equal(t, int64(1), a.conta(t, `SELECT count(*) FROM outbox_events
		WHERE aggregate_id = ? AND event_type = 'WagerTransactionProcessed'
		  AND payload->>'transactionId' = ?`,
		uuid.UUID(w), pendente.TransactionID.String()),
		"a conclusão pelo worker emite evento como qualquer outra")
}

// AC-6
func TestPendenciaEsgotadaTerminaComReferenceNotFound(t *testing.T) {
	a := novoAmbienteReversao(t)
	w, p := a.carteira(t, "100.00")

	pendente, err := a.processor.Process(a.ctx,
		reversao(t, w, p, "rf-1", domain.Refund, "40.00", "nunca-chega"))
	require.NoError(t, err)
	require.Equal(t, domain.PendingReference, pendente.Status)

	// Uma rodada por tentativa, andando o relógio além do backoff de cada uma.
	for i := 1; i <= appwagering.MaxResolveAttempts; i++ {
		a.relogio.avancar(appwagering.Backoff(i) + time.Second)
		if _, err := a.resolver.RunOnce(a.ctx); err != nil {
			t.Fatalf("rodada %d: %v", i, err)
		}
	}

	assert.Equal(t, int64(1), a.conta(t, `SELECT count(*) FROM wager_transactions
		WHERE id = ? AND status = 'REJECTED' AND failure_code = 'REFERENCE_NOT_FOUND'`,
		uuid.UUID(pendente.TransactionID)),
		"a pendência não pode ficar viva para sempre")
	assert.Equal(t, "100.00 BRL", a.saldo(t, w), "desistir não movimenta saldo")
	assert.Equal(t, int64(1), a.conta(t, `SELECT count(*) FROM outbox_events
		WHERE aggregate_id = ? AND event_type = 'WagerTransactionRejected'`, uuid.UUID(w)),
		"quem esperava a resposta precisa saber que ela chegou")
}

// AC-7
func TestReversaoDeReferenciaRecusadaERecusada(t *testing.T) {
	a := novoAmbienteReversao(t)
	w, p := a.carteira(t, "100.00")

	recusada, err := a.processor.Process(a.ctx, operacao(t, w, p, "bet-1", domain.Bet, "500.00"))
	require.NoError(t, err)
	require.Equal(t, domain.Rejected, recusada.Status)

	out, err := a.processor.Process(a.ctx,
		reversao(t, w, p, "rf-1", domain.Refund, "500.00", "bet-1"))

	require.NoError(t, err)
	assert.Equal(t, domain.Rejected, out.Status)
	assert.Equal(t, domain.FailureReferenceNotProcessed, out.FailureCode,
		"não há o que estornar: a aposta nunca saiu da carteira")
}

// AC-8
func TestReversaoDeReferenciaAindaPendenteAguarda(t *testing.T) {
	a := novoAmbienteReversao(t)
	w, p := a.carteira(t, "100.00")

	primeira, err := a.processor.Process(a.ctx,
		reversao(t, w, p, "rf-1", domain.Refund, "40.00", "bet-1"))
	require.NoError(t, err)
	require.Equal(t, domain.PendingReference, primeira.Status)

	// Um estorno do estorno pendente: o desfecho da referência ainda pode mudar,
	// então recusar agora faria a ordem de chegada virar regra de negócio.
	out, err := a.processor.Process(a.ctx,
		reversao(t, w, p, "rb-1", domain.Rollback, "40.00", "rf-1"))

	require.NoError(t, err)
	assert.Equal(t, domain.PendingReference, out.Status)
	assert.NotEqual(t, domain.FailureReferenceNotProcessed, out.FailureCode)
}

// AC-9
func TestDivergenciaEntreOperacaoEReferenciaRecusa(t *testing.T) {
	a := novoAmbienteReversao(t)
	w, p := a.carteira(t, "100.00")
	outraW, outroP := a.carteira(t, "100.00")

	a.processada(t, operacao(t, w, p, "bet-1", domain.Bet, "40.00"))

	casos := map[string]func(appwagering.Input) appwagering.Input{
		"outra carteira e outro jogador": func(in appwagering.Input) appwagering.Input {
			in.WalletID, in.PlayerID = outraW, outroP
			return in
		},
		"outra rodada": func(in appwagering.Input) appwagering.Input {
			in.RoundID = "round-outra"
			return in
		},
	}

	i := 0
	for nome, ajustar := range casos {
		i++
		t.Run(nome, func(t *testing.T) {
			base := reversao(t, w, p, "rf-div", domain.Refund, "40.00", "bet-1")
			base.ExternalID = domain.ExternalID("rf-div-" + string(rune('a'+i)))
			base.IdempotencyKey = "provider-a:" + string(base.ExternalID)

			out, err := a.processor.Process(a.ctx, ajustar(base))
			require.NoError(t, err)
			assert.Equal(t, domain.Rejected, out.Status)
			assert.Equal(t, domain.FailureReferenceMismatch, out.FailureCode)
		})
	}
}

// AC-10
func TestValorDiferenteDoReferenciadoRecusa(t *testing.T) {
	a := novoAmbienteReversao(t)
	w, p := a.carteira(t, "100.00")

	a.processada(t, operacao(t, w, p, "bet-1", domain.Bet, "40.00"))

	out, err := a.processor.Process(a.ctx,
		reversao(t, w, p, "rf-1", domain.Refund, "39.99", "bet-1"))

	require.NoError(t, err)
	assert.Equal(t, domain.Rejected, out.Status)
	assert.Equal(t, domain.FailureAmountMismatch, out.FailureCode,
		"estorno parcial não é suportado, e silenciar a diferença perderia dinheiro")
	assert.Equal(t, "60.00 BRL", a.saldo(t, w))
}

// AC-11
func TestReversaoQueDeixariaSaldoNegativoRecusaComCodigoProprio(t *testing.T) {
	a := novoAmbienteReversao(t)
	w, p := a.carteira(t, "100.00")

	a.processada(t, operacao(t, w, p, "win-1", domain.Win, "50.00"))
	a.processada(t, operacao(t, w, p, "bet-1", domain.Bet, "140.00"))

	out, err := a.processor.Process(a.ctx,
		reversao(t, w, p, "rb-1", domain.Rollback, "50.00", "win-1"))

	require.NoError(t, err)
	assert.Equal(t, domain.Rejected, out.Status)
	assert.Equal(t, domain.FailureReversalInsufficientFunds, out.FailureCode,
		"§7 pede código distinto do de aposta sem saldo: aqui o dinheiro já foi gasto")
	assert.NotEqual(t, domain.FailureInsufficientFunds, out.FailureCode)
	assert.Equal(t, "10.00 BRL", a.saldo(t, w))
}

// AC-E1, AC-E2, AC-E3, AC-E4
func TestTiposQueNaoPodemSerRevertidos(t *testing.T) {
	a := novoAmbienteReversao(t)
	w, p := a.carteira(t, "1000.00")

	a.processada(t, operacao(t, w, p, "win-1", domain.Win, "50.00"))
	// LOSS exige valor zero: a perda não movimenta a carteira, o débito já
	// aconteceu na aposta. É por isso mesmo que ela não é reversível.
	a.processada(t, operacao(t, w, p, "loss-1", domain.Loss, "0.00"))
	a.processada(t, operacao(t, w, p, "bet-1", domain.Bet, "30.00"))
	a.processada(t, reversao(t, w, p, "rb-1", domain.Rollback, "30.00", "bet-1"))

	casos := []struct {
		nome, externo, referencia, montante string
		kind                                domain.Kind
	}{
		{"AC-E1 refund de um ganho", "x1", "win-1", "50.00", domain.Refund},
		{"AC-E2 rollback de uma perda", "x2", "loss-1", "10.00", domain.Rollback},
		{"AC-E3 rollback de um rollback", "x3", "rb-1", "30.00", domain.Rollback},
		{"AC-E4 reversão de si mesma", "x4", "x4", "10.00", domain.Rollback},
	}

	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			out, err := a.processor.Process(a.ctx,
				reversao(t, w, p, c.externo, c.kind, c.montante, c.referencia))
			require.NoError(t, err)
			assert.Equal(t, domain.Rejected, out.Status)
			assert.Equal(t, domain.FailureReferenceMismatch, out.FailureCode)
		})
	}
}

// AC-E6
func TestReplayDePendenciaJaResolvidaDevolveODesfechoPersistido(t *testing.T) {
	a := novoAmbienteReversao(t)
	w, p := a.carteira(t, "100.00")

	entrada := reversao(t, w, p, "rf-1", domain.Refund, "80.00", "bet-1")
	pendente, err := a.processor.Process(a.ctx, entrada)
	require.NoError(t, err)
	require.Equal(t, domain.PendingReference, pendente.Status)

	a.processada(t, operacao(t, w, p, "bet-1", domain.Bet, "80.00"))
	a.relogio.avancar(appwagering.Backoff(1))
	_, err = a.resolver.RunOnce(a.ctx)
	require.NoError(t, err)

	// O provedor reenvia a mesma operação sem saber que o worker já concluiu.
	replay, err := a.processor.Process(a.ctx, entrada)

	require.NoError(t, err)
	assert.True(t, replay.IdempotentReplay)
	assert.Equal(t, domain.Processed, replay.Status,
		"o reenvio tem que enxergar o desfecho novo, não o 202 antigo")
	assert.Equal(t, pendente.TransactionID, replay.TransactionID)
	assert.Equal(t, "100.00 BRL", replay.Balance.String())
	assert.Equal(t, int64(1), a.conta(t, `SELECT count(*) FROM wallet_ledger_entries
		WHERE transaction_id = ?`, uuid.UUID(pendente.TransactionID)),
		"o reenvio não pode gerar um segundo lançamento")
}

// Segurança — a referência é buscada pelo provedor do TOKEN, nunca pelo do
// corpo. Sem isso, bastaria conhecer o identificador externo alheio para
// estornar a aposta de outro provedor e creditar a carteira.
//
// O worker entra no teste de propósito: ele é um segundo caminho até a mesma
// decisão, e um isolamento que só valesse no fluxo síncrono seria falso.
//
// A resposta esperada é PENDING_REFERENCE, e não uma recusa: para provider-b
// aquela referência não existe, e é isso que ele tem de ouvir. Um
// REFERENCE_MISMATCH confirmaria que a transação de provider-a existe — a
// mesma razão pela qual consulta de outro provedor responde 404 e não 403.
// Verificado por mutação: removido o filtro por provedor da busca, a resposta
// vira REJECTED e este teste fica vermelho.
func TestProvedorNaoReverteAApostaDeOutro(t *testing.T) {
	a := novoAmbienteReversao(t)
	w, p := a.carteira(t, "100.00")

	aposta := a.processada(t, operacao(t, w, p, "bet-1", domain.Bet, "80.00"))

	invasor := reversao(t, w, p, "rf-1", domain.Refund, "80.00", "bet-1")
	invasor.ProviderID = "provider-b"
	invasor.IdempotencyKey = "provider-b:rf-1"

	out, err := a.processor.Process(a.ctx, invasor)
	require.NoError(t, err)
	require.NotEqual(t, domain.Processed, out.Status,
		"provider-b não pode estornar a aposta de provider-a")
	assert.Equal(t, domain.PendingReference, out.Status,
		"para provider-b aquela referência simplesmente não existe")

	// E continua não existindo por mais que o worker insista.
	for i := 1; i <= appwagering.MaxResolveAttempts; i++ {
		a.relogio.avancar(appwagering.Backoff(i) + time.Second)
		if _, err := a.resolver.RunOnce(a.ctx); err != nil {
			t.Fatalf("rodada %d: %v", i, err)
		}
	}

	assert.Equal(t, int64(1), a.conta(t, `SELECT count(*) FROM wager_transactions
		WHERE id = ? AND status = 'REJECTED' AND failure_code = 'REFERENCE_NOT_FOUND'`,
		uuid.UUID(out.TransactionID)))
	assert.Equal(t, "20.00 BRL", a.saldo(t, w),
		"a carteira não pode se mexer por causa de uma reversão alheia")
	assert.Zero(t, a.conta(t, `SELECT count(*) FROM wager_transactions
		WHERE reference_transaction_id = ?`, uuid.UUID(aposta.TransactionID)),
		"a aposta de provider-a segue sem reversão associada")
}
