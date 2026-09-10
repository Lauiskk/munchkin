//go:build integration

package integration_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/internal/adapter/postgres"
	"github.com/Lauiskk/munchkin/internal/app"
	appwagering "github.com/Lauiskk/munchkin/internal/app/wagering"
	appwallet "github.com/Lauiskk/munchkin/internal/app/wallet"
	"github.com/Lauiskk/munchkin/internal/domain/money"
	domain "github.com/Lauiskk/munchkin/internal/domain/wagering"
	domainwallet "github.com/Lauiskk/munchkin/internal/domain/wallet"
)

type ambienteOperacao struct {
	ambiente
	processor *appwagering.Processor
	querier   *appwagering.Querier
	metricas  *registroDeMetricas
}

func novoAmbienteOperacao(t *testing.T) ambienteOperacao {
	t.Helper()
	a := novoAmbiente(t)
	transacoes := postgres.NewTransactionRepository(a.db)
	metricas := &registroDeMetricas{}
	return ambienteOperacao{
		ambiente: a,
		processor: appwagering.NewProcessor(a.db,
			postgres.NewWalletRepository(a.db), transacoes,
			postgres.NewLedgerRepository(a.db), postgres.NewOutboxRepository(a.db),
			relogioFixo{t: agoraFixo}, metricas),
		querier:  appwagering.NewQuerier(transacoes),
		metricas: metricas,
	}
}

func (a ambienteOperacao) carteira(t *testing.T, saldo string) (domainwallet.ID, domainwallet.PlayerID) {
	t.Helper()
	jogador := jogadorNovo(t)
	out, err := a.opener.Open(a.ctx, appwallet.OpenInput{
		PlayerID: jogador, InitialBalance: valor(t, saldo),
	})
	require.NoError(t, err)
	return out.Wallet.ID(), jogador
}

func operacao(t *testing.T, w domainwallet.ID, p domainwallet.PlayerID,
	externo string, kind domain.Kind, montante string) appwagering.Input {
	t.Helper()
	return appwagering.Input{
		ProviderID: "provider-a", ExternalID: domain.ExternalID(externo),
		IdempotencyKey: "provider-a:" + externo,
		PlayerID:       p, WalletID: w,
		RoundID: "round-1", GameID: "fortune-chimp",
		Kind: kind, Money: valor(t, montante),
	}
}

// saldo devolve o saldo atual da carteira formatado como o domínio o expõe.
func (a ambienteOperacao) saldo(t *testing.T, w domainwallet.ID) string {
	t.Helper()
	var minor int64
	require.NoError(t, a.db.Session(a.ctx).
		Raw(`SELECT balance_minor FROM wallets WHERE id = ?`, uuid.UUID(w)).
		Scan(&minor).Error)
	v, err := money.New(minor, money.BRL)
	require.NoError(t, err)
	return v.String()
}

func (a ambienteOperacao) conta(t *testing.T, q string, args ...any) int64 {
	t.Helper()
	var n int64
	require.NoError(t, a.db.Session(a.ctx).Raw(q, args...).Scan(&n).Error)
	return n
}

// AC-1
func TestApostaProcessadaGravaTudoNoMesmoCommit(t *testing.T) {
	a := novoAmbienteOperacao(t)
	w, p := a.carteira(t, "1000.00")

	out, err := a.processor.Process(a.ctx, operacao(t, w, p, "tx-1", domain.Bet, "25.00"))
	require.NoError(t, err)

	assert.Equal(t, domain.Processed, out.Status)
	assert.Equal(t, "975.00", out.Balance.Amount())
	assert.False(t, out.IdempotentReplay)

	id := uuid.UUID(w)
	assert.Equal(t, int64(1), a.conta(t, `SELECT count(*) FROM wallet_ledger_entries
		WHERE wallet_id = ? AND direction = 'DEBIT'`, id))
	// Abertura (2) + esta operação (2) = 4 eventos pendentes.
	assert.Equal(t, int64(4), a.conta(t,
		`SELECT count(*) FROM outbox_events WHERE aggregate_id = ?`, id))
}

// AC-2, AC-8: o replay devolve o saldo do processamento ORIGINAL, mesmo depois
// de a carteira ter se movimentado. É o que o contrato promete, e é o que
// impede o provedor de concluir que o saldo mudou sozinho.
func TestReplayDevolveOSaldoDoProcessamentoOriginal(t *testing.T) {
	a := novoAmbienteOperacao(t)
	w, p := a.carteira(t, "1000.00")

	primeira, err := a.processor.Process(a.ctx, operacao(t, w, p, "tx-1", domain.Bet, "25.00"))
	require.NoError(t, err)
	require.Equal(t, "975.00", primeira.Balance.Amount())

	// A carteira se movimenta depois.
	_, err = a.processor.Process(a.ctx, operacao(t, w, p, "tx-2", domain.Bet, "100.00"))
	require.NoError(t, err)

	replay, err := a.processor.Process(a.ctx, operacao(t, w, p, "tx-1", domain.Bet, "25.00"))
	require.NoError(t, err)

	assert.True(t, replay.IdempotentReplay)
	assert.Equal(t, primeira.TransactionID, replay.TransactionID)
	assert.Equal(t, "975.00", replay.Balance.Amount(),
		"o saldo do replay é o observado no processamento original, não o atual")

	assert.Equal(t, int64(875_00), a.conta(t,
		"SELECT balance_minor FROM wallets WHERE id = ?", uuid.UUID(w)),
		"o replay não pode ter reaplicado nada")
	assert.Equal(t, int64(2), a.conta(t, `SELECT count(*) FROM wallet_ledger_entries
		WHERE wallet_id = ? AND direction = 'DEBIT'`, uuid.UUID(w)))
}

// AC-3, AC-4
func TestConflitosDeIdempotencia(t *testing.T) {
	a := novoAmbienteOperacao(t)
	w, p := a.carteira(t, "1000.00")

	_, err := a.processor.Process(a.ctx, operacao(t, w, p, "tx-1", domain.Bet, "25.00"))
	require.NoError(t, err)

	t.Run("mesma chave, conteúdo diferente", func(t *testing.T) {
		in := operacao(t, w, p, "tx-1", domain.Bet, "99.00")
		_, err := a.processor.Process(a.ctx, in)
		require.Error(t, err)
		assert.ErrorIs(t, err, appwagering.ErrIdempotencyConflict)
	})

	t.Run("mesma operação, outra chave", func(t *testing.T) {
		in := operacao(t, w, p, "tx-1", domain.Bet, "25.00")
		in.IdempotencyKey = "outra-chave"
		_, err := a.processor.Process(a.ctx, in)
		require.Error(t, err)
		assert.ErrorIs(t, err, appwagering.ErrIdempotencyConflict)
	})

	assert.Equal(t, int64(1), a.conta(t, `SELECT count(*) FROM wager_transactions
		WHERE wallet_id = ? AND provider_id IS NOT NULL`, uuid.UUID(w)),
		"nenhum conflito pode ter gravado registro")
}

// AC-5: a recusa por saldo COMMITA. Sem isso o provedor reenviaria para sempre
// uma operação que já tem desfecho.
func TestRecusaPorSaldoEhPersistidaEAuditavel(t *testing.T) {
	a := novoAmbienteOperacao(t)
	w, p := a.carteira(t, "10.00")

	out, err := a.processor.Process(a.ctx, operacao(t, w, p, "tx-big", domain.Bet, "500.00"))
	require.NoError(t, err, "recusa de negócio não é erro do caso de uso")

	assert.Equal(t, domain.Rejected, out.Status)
	assert.Equal(t, domain.FailureInsufficientFunds, out.FailureCode)
	assert.Equal(t, "10.00", out.Balance.Amount())

	id := uuid.UUID(w)
	assert.Equal(t, int64(1000), a.conta(t,
		"SELECT balance_minor FROM wallets WHERE id = ?", id), "o saldo não mudou")
	assert.Equal(t, int64(1), a.conta(t, `SELECT version FROM wallets WHERE id = ?`, id),
		"a versão não avança numa recusa")
	assert.Equal(t, int64(1), a.conta(t, `SELECT count(*) FROM wager_transactions
		WHERE wallet_id = ? AND status = 'REJECTED' AND failure_code = 'INSUFFICIENT_FUNDS'`, id))
	assert.Equal(t, int64(1), a.conta(t, `SELECT count(*) FROM outbox_events
		WHERE aggregate_id = ? AND event_type = 'WagerTransactionRejected'`, id))

	// E o reenvio da mesma operação recusada devolve o mesmo desfecho, sem
	// tentar de novo.
	replay, err := a.processor.Process(a.ctx, operacao(t, w, p, "tx-big", domain.Bet, "500.00"))
	require.NoError(t, err)
	assert.True(t, replay.IdempotentReplay)
	assert.Equal(t, domain.Rejected, replay.Status)
	assert.Equal(t, domain.FailureInsufficientFunds, replay.FailureCode)
}

// AC-7: LOSS conclui sem movimentar. Produz o evento de conclusão e NÃO produz
// o de mudança de saldo — emiti-lo faria um consumidor registrar uma mudança
// que não houve.
func TestPerdaConcluiSemMovimentarACarteira(t *testing.T) {
	a := novoAmbienteOperacao(t)
	w, p := a.carteira(t, "100.00")
	id := uuid.UUID(w)

	antes := a.conta(t, "SELECT version FROM wallets WHERE id = ?", id)

	out, err := a.processor.Process(a.ctx, operacao(t, w, p, "loss-1", domain.Loss, "0.00"))
	require.NoError(t, err)

	assert.Equal(t, domain.Processed, out.Status)
	assert.Equal(t, "100.00", out.Balance.Amount())
	assert.Equal(t, antes, a.conta(t, "SELECT version FROM wallets WHERE id = ?", id),
		"LOSS não altera a versão da carteira")
	assert.Equal(t, int64(1), a.conta(t,
		"SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = ?", id),
		"apenas o lançamento da abertura: LOSS não gera lançamento")

	assert.Equal(t, int64(1), a.conta(t, `SELECT count(*) FROM outbox_events
		WHERE aggregate_id = ? AND event_type = 'WagerTransactionProcessed'
		  AND payload->>'kind' = 'LOSS'`, id))
	assert.Equal(t, int64(1), a.conta(t, `SELECT count(*) FROM outbox_events
		WHERE aggregate_id = ? AND event_type = 'WalletBalanceChanged'`, id),
		"só o da abertura: LOSS não produz mudança de saldo")
}

// AC-6
func TestGanhoCreditaEAvancaAVersao(t *testing.T) {
	a := novoAmbienteOperacao(t)
	w, p := a.carteira(t, "100.00")

	out, err := a.processor.Process(a.ctx, operacao(t, w, p, "win-1", domain.Win, "50.00"))
	require.NoError(t, err)

	assert.Equal(t, domain.Processed, out.Status)
	assert.Equal(t, "150.00", out.Balance.Amount())
	assert.Equal(t, int64(2), a.conta(t,
		"SELECT version FROM wallets WHERE id = ?", uuid.UUID(w)))
}

// AC-E2, AC-E3, AC-E4, AC-E5: recusas de alvo inválido não são persistidas.
// Uma operação que não endereça uma carteira válida não é dado de auditoria, é
// entrada errada — e, no caso da moeda, a chave estrangeira composta do schema
// literalmente não a comporta.
func TestRecusasDeAlvoInvalidoNaoSaoPersistidas(t *testing.T) {
	a := novoAmbienteOperacao(t)
	w, p := a.carteira(t, "100.00")
	outroJogador := jogadorNovo(t)
	carteiraInexistente, err := domainwallet.NewID()
	require.NoError(t, err)

	dolares, err := money.Parse("10.00", money.USD)
	require.NoError(t, err)

	casos := map[string]struct {
		ajustar func(*appwagering.Input)
		codigo  domain.FailureCode
	}{
		"carteira inexistente": {
			func(in *appwagering.Input) { in.WalletID = carteiraInexistente },
			domain.FailureWalletNotFound,
		},
		"carteira de outro jogador": {
			func(in *appwagering.Input) { in.PlayerID = outroJogador },
			domain.FailureWalletPlayerMismatch,
		},
		"moeda diferente da carteira": {
			func(in *appwagering.Input) { in.Money = dolares },
			domain.FailureCurrencyMismatch,
		},
		"aposta de valor zero": {
			func(in *appwagering.Input) { in.Money = valor(t, "0.00") },
			domain.FailureInvalidAmountForKind,
		},
	}
	for nome, caso := range casos {
		t.Run(nome, func(t *testing.T) {
			in := operacao(t, w, p, "alvo-"+nome, domain.Bet, "10.00")
			caso.ajustar(&in)

			out, err := a.processor.Process(a.ctx, in)
			require.NoError(t, err)
			assert.Equal(t, domain.Rejected, out.Status)
			assert.Equal(t, caso.codigo, out.FailureCode)
			assert.True(t, out.TransactionID.IsZero(), "recusa de alvo não gera registro")
		})
	}

	assert.Equal(t, int64(100_00), a.conta(t,
		"SELECT balance_minor FROM wallets WHERE id = ?", uuid.UUID(w)),
		"nenhuma recusa pode ter mexido no saldo")
}

// AC-13: um provedor não descobre a existência de transação de outro. A resposta
// é "não encontrado", e não "proibido": 403 confirmaria que existe.
func TestProvedorNaoEnxergaTransacaoDeOutro(t *testing.T) {
	a := novoAmbienteOperacao(t)
	w, p := a.carteira(t, "100.00")

	out, err := a.processor.Process(a.ctx, operacao(t, w, p, "tx-1", domain.Bet, "10.00"))
	require.NoError(t, err)

	t.Run("pelo identificador interno", func(t *testing.T) {
		_, err := a.querier.ByID(a.ctx, out.TransactionID, "provider-b")
		require.Error(t, err)
		assert.ErrorIs(t, err, app.ErrNotFound)

		encontrada, err := a.querier.ByID(a.ctx, out.TransactionID, "provider-a")
		require.NoError(t, err)
		assert.Equal(t, out.TransactionID, encontrada.ID())
	})

	t.Run("pelo identificador externo", func(t *testing.T) {
		_, err := a.querier.ByExternalID(a.ctx, "provider-b", "tx-1")
		require.Error(t, err)
		assert.ErrorIs(t, err, app.ErrNotFound)

		encontrada, err := a.querier.ByExternalID(a.ctx, "provider-a", "tx-1")
		require.NoError(t, err)
		assert.Equal(t, out.TransactionID, encontrada.ID())
	})
}

// TestCreditoQueEstouraORepresentavelEhRecusaENaoDefeito guarda o desfecho de um
// crédito que não cabe no int64.
//
// Achado numa passada de QA: a carteira podia ser aberta no teto exato
// (92.233.720.368.547.758,07) e um WIN de um centavo sobre ela devolvia 500
// INTERNAL_ERROR. A entrada era válida, a carteira existia e o resultado era o
// mesmo toda vez — tudo o que descreve uma RECUSA. Um 500 ali diz a quem integra
// "há um defeito, não repita" sobre algo determinístico, e some do relatório de
// recusas por código de falha.
func TestCreditoQueEstouraORepresentavelEhRecusaENaoDefeito(t *testing.T) {
	a := novoAmbienteOperacao(t)
	const teto = "92233720368547758.07"
	w, p := a.carteira(t, teto)

	out, err := a.processor.Process(a.ctx, operacao(t, w, p, "estouro-1", domain.Win, "0.01"))

	require.NoError(t, err, "estouro é recusa de negócio, não erro devolvido ao chamador")
	assert.Equal(t, domain.Rejected, out.Status)
	assert.Equal(t, domain.FailureBalanceLimitExceeded, out.FailureCode)
	assert.Equal(t, teto+" BRL", a.saldo(t, w), "a recusa não pode encostar no saldo")

	// A recusa é persistida e auditável como qualquer outra, e o reenvio devolve
	// o mesmo desfecho em vez de tentar de novo.
	repetida, err := a.processor.Process(a.ctx, operacao(t, w, p, "estouro-1", domain.Win, "0.01"))
	require.NoError(t, err)
	assert.True(t, repetida.IdempotentReplay)
	assert.Equal(t, domain.FailureBalanceLimitExceeded, repetida.FailureCode)
}

// TestReversaoQueEstouraORepresentavelTambemEhRecusa cobre o outro caminho que
// credita: REFUND devolve dinheiro, e devolver também pode não caber.
func TestReversaoQueEstouraORepresentavelTambemEhRecusa(t *testing.T) {
	a := novoAmbienteOperacao(t)
	const teto = "92233720368547758.07"
	w, p := a.carteira(t, teto)

	// Debita, para caber; devolve, para não caber de novo — com a carteira
	// recreditada ao teto no meio do caminho.
	_, err := a.processor.Process(a.ctx, operacao(t, w, p, "aposta-1", domain.Bet, "10.00"))
	require.NoError(t, err)
	_, err = a.processor.Process(a.ctx, operacao(t, w, p, "ganho-1", domain.Win, "10.00"))
	require.NoError(t, err)

	entrada := operacao(t, w, p, "devolucao-1", domain.Refund, "10.00")
	entrada.ReferenceExternalID = "aposta-1"
	out, err := a.processor.Process(a.ctx, entrada)

	require.NoError(t, err)
	assert.Equal(t, domain.Rejected, out.Status)
	assert.Equal(t, domain.FailureBalanceLimitExceeded, out.FailureCode)
	assert.Equal(t, teto+" BRL", a.saldo(t, w))
}
