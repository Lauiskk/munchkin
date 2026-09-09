//go:build integration

package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/internal/adapter/postgres"
	"github.com/Lauiskk/munchkin/internal/app"
	appwallet "github.com/Lauiskk/munchkin/internal/app/wallet"
	"github.com/Lauiskk/munchkin/internal/domain/event"
	"github.com/Lauiskk/munchkin/internal/domain/ledger"
	"github.com/Lauiskk/munchkin/internal/domain/money"
	"github.com/Lauiskk/munchkin/internal/domain/wagering"
	domainwallet "github.com/Lauiskk/munchkin/internal/domain/wallet"
)

// relogioFixo torna o instante previsível, para que dois registros gravados na
// mesma operação possam ser comparados.
type relogioFixo struct{ t time.Time }

func (r relogioFixo) Now() time.Time { return r.t }

type ambiente struct {
	db     *postgres.Database
	opener *appwallet.Opener
	getter *appwallet.Getter
	ctx    context.Context
}

func novoAmbiente(t *testing.T) ambiente {
	t.Helper()
	dbCfg := migrado(t)
	db := abrir(t, dbCfg)

	wallets := postgres.NewWalletRepository(db)
	transacoes := postgres.NewTransactionRepository(db)
	entradas := postgres.NewLedgerRepository(db)
	outbox := postgres.NewOutboxRepository(db)
	relogio := relogioFixo{t: time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)}

	return ambiente{
		db:     db,
		opener: appwallet.NewOpener(db, wallets, transacoes, entradas, outbox, relogio),
		getter: appwallet.NewGetter(wallets),
		ctx:    context.Background(),
	}
}

func valor(t *testing.T, v string) money.Money {
	t.Helper()
	m, err := money.Parse(v, money.BRL)
	require.NoError(t, err)
	return m
}

func jogadorNovo(t *testing.T) domainwallet.PlayerID {
	t.Helper()
	p, err := domainwallet.NewPlayerID()
	require.NoError(t, err)
	return p
}

// contar devolve quantas linhas existem na tabela para aquela carteira.
func (a ambiente) contar(t *testing.T, tabela, coluna string, id uuid.UUID) int64 {
	t.Helper()
	var n int64
	require.NoError(t, a.db.Session(a.ctx).
		Raw("SELECT count(*) FROM "+tabela+" WHERE "+coluna+" = ?", id).Scan(&n).Error)
	return n
}

// AC-1, AC-2: a abertura com saldo positivo grava carteira, transação de
// abertura, lançamento e DOIS eventos — tudo no mesmo commit.
func TestAberturaComSaldoGravaTudoNoMesmoCommit(t *testing.T) {
	a := novoAmbiente(t)

	out, err := a.opener.Open(a.ctx, appwallet.OpenInput{
		PlayerID:       jogadorNovo(t),
		InitialBalance: valor(t, "1000.00"),
	})
	require.NoError(t, err)

	w := out.Wallet
	assert.Equal(t, "1000.00", w.Balance().Amount())
	assert.Equal(t, int64(1), w.Version(), "o crédito de abertura faz parte da criação")

	id := uuid.UUID(w.ID())
	assert.Equal(t, int64(1), a.contar(t, "wallets", "id", id))
	assert.Equal(t, int64(1), a.contar(t, "wager_transactions", "wallet_id", id))
	assert.Equal(t, int64(1), a.contar(t, "wallet_ledger_entries", "wallet_id", id))
	assert.Equal(t, int64(2), a.contar(t, "outbox_events", "aggregate_id", id),
		"WagerTransactionProcessed e WalletBalanceChanged")

	var estado struct {
		Kind               string
		Status             string
		ResultBalanceMinor int64
		ProviderID         *string
	}
	require.NoError(t, a.db.Session(a.ctx).Raw(`
		SELECT kind, status, result_balance_minor, provider_id
		  FROM wager_transactions WHERE wallet_id = ?`, id).Scan(&estado).Error)

	assert.Equal(t, "OPENING", estado.Kind)
	assert.Equal(t, "PROCESSED", estado.Status)
	assert.Equal(t, int64(100000), estado.ResultBalanceMinor)
	assert.Nil(t, estado.ProviderID, "abertura interna não carrega provedor")

	var lancamento struct {
		Direction          string
		AmountMinor        int64
		BalanceBeforeMinor int64
		BalanceAfterMinor  int64
	}
	require.NoError(t, a.db.Session(a.ctx).Raw(`
		SELECT direction, amount_minor, balance_before_minor, balance_after_minor
		  FROM wallet_ledger_entries WHERE wallet_id = ?`, id).Scan(&lancamento).Error)

	assert.Equal(t, "CREDIT", lancamento.Direction)
	assert.Equal(t, int64(100000), lancamento.AmountMinor)
	assert.Equal(t, int64(0), lancamento.BalanceBeforeMinor)
	assert.Equal(t, int64(100000), lancamento.BalanceAfterMinor)

	var tipos []string
	require.NoError(t, a.db.Session(a.ctx).Raw(`
		SELECT event_type FROM outbox_events WHERE aggregate_id = ? ORDER BY event_type`,
		id).Scan(&tipos).Error)
	assert.Equal(t, []string{"WagerTransactionProcessed", "WalletBalanceChanged"}, tipos)

	// Nenhum evento foi publicado: publicar é trabalho de outro worker, depois
	// do commit. Todos ficam pendentes.
	pendentes, err := postgres.NewOutboxRepository(a.db).CountPending(a.ctx, id)
	require.NoError(t, err)
	assert.Equal(t, int64(2), pendentes)
}

// AC-3
func TestAberturaComSaldoZeroGravaApenasACarteira(t *testing.T) {
	a := novoAmbiente(t)

	out, err := a.opener.Open(a.ctx, appwallet.OpenInput{
		PlayerID:       jogadorNovo(t),
		InitialBalance: valor(t, "0.00"),
	})
	require.NoError(t, err)

	id := uuid.UUID(out.Wallet.ID())
	assert.Equal(t, int64(1), a.contar(t, "wallets", "id", id))
	assert.Zero(t, a.contar(t, "wager_transactions", "wallet_id", id))
	assert.Zero(t, a.contar(t, "wallet_ledger_entries", "wallet_id", id))
	assert.Zero(t, a.contar(t, "outbox_events", "aggregate_id", id),
		"saldo inicial zero não produz evento financeiro")
}

// AC-4: a unicidade é decidida pelo índice, não por consulta prévia — e a
// segunda tentativa não pode deixar nada para trás.
func TestSegundaAberturaParaOMesmoJogadorEhConflito(t *testing.T) {
	a := novoAmbiente(t)
	jogador := jogadorNovo(t)

	primeira, err := a.opener.Open(a.ctx, appwallet.OpenInput{
		PlayerID: jogador, InitialBalance: valor(t, "100.00"),
	})
	require.NoError(t, err)

	_, err = a.opener.Open(a.ctx, appwallet.OpenInput{
		PlayerID: jogador, InitialBalance: valor(t, "500.00"),
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, domainwallet.ErrAlreadyExists)

	var carteiras int64
	require.NoError(t, a.db.Session(a.ctx).Raw(
		"SELECT count(*) FROM wallets WHERE player_id = ?", uuid.UUID(jogador)).
		Scan(&carteiras).Error)
	assert.Equal(t, int64(1), carteiras, "a segunda tentativa não pode ter gravado nada")

	// E a primeira continua intacta.
	w, err := a.getter.Get(a.ctx, primeira.Wallet.ID())
	require.NoError(t, err)
	assert.Equal(t, "100.00", w.Balance().Amount())
}

// AC-9: é a base da reconciliação. Créditos menos débitos tem de dar o saldo.
func TestSomaDosLancamentosReconstroiOSaldo(t *testing.T) {
	a := novoAmbiente(t)

	out, err := a.opener.Open(a.ctx, appwallet.OpenInput{
		PlayerID: jogadorNovo(t), InitialBalance: valor(t, "1234.56"),
	})
	require.NoError(t, err)

	total, lancamentos, err := postgres.NewLedgerRepository(a.db).
		SumSigned(a.ctx, uuid.UUID(out.Wallet.ID()))
	require.NoError(t, err)

	assert.Equal(t, int64(1), lancamentos)
	assert.Equal(t, out.Wallet.Balance().Minor(), total)
}

// AC-7, AC-8
func TestConsultaDeCarteira(t *testing.T) {
	a := novoAmbiente(t)

	out, err := a.opener.Open(a.ctx, appwallet.OpenInput{
		PlayerID: jogadorNovo(t), InitialBalance: valor(t, "42.00"),
	})
	require.NoError(t, err)

	t.Run("existente", func(t *testing.T) {
		w, err := a.getter.Get(a.ctx, out.Wallet.ID())
		require.NoError(t, err)
		assert.Equal(t, out.Wallet.ID(), w.ID())
		assert.Equal(t, "42.00", w.Balance().Amount())
		assert.Equal(t, int64(1), w.Version())
	})

	t.Run("inexistente", func(t *testing.T) {
		outra, err := domainwallet.NewID()
		require.NoError(t, err)
		_, err = a.getter.Get(a.ctx, outra)
		require.Error(t, err)
		assert.ErrorIs(t, err, app.ErrNotFound,
			"carteira inexistente precisa ser distinguível de falha de banco")
	})
}

// AC-E5: falha no meio da abertura desfaz TUDO. Não pode restar carteira sem a
// transação que a creditou, nem transação sem lançamento.
func TestFalhaNoMeioDaAberturaDesfazTudo(t *testing.T) {
	a := novoAmbiente(t)
	jogador := jogadorNovo(t)

	// A outbox falha de propósito, depois de a carteira, a transação e o
	// lançamento já terem sido inseridos na transação corrente.
	falha := errors.New("falha proposital na outbox")
	opener := appwallet.NewOpener(a.db,
		postgres.NewWalletRepository(a.db),
		postgres.NewTransactionRepository(a.db),
		postgres.NewLedgerRepository(a.db),
		outboxQueFalha{err: falha},
		relogioFixo{t: time.Now().UTC()},
	)

	_, err := opener.Open(a.ctx, appwallet.OpenInput{
		PlayerID: jogador, InitialBalance: valor(t, "100.00"),
	})
	require.ErrorIs(t, err, falha)

	var carteiras int64
	require.NoError(t, a.db.Session(a.ctx).Raw(
		"SELECT count(*) FROM wallets WHERE player_id = ?", uuid.UUID(jogador)).
		Scan(&carteiras).Error)
	assert.Zero(t, carteiras, "a carteira não pode ter sobrevivido ao desfazimento")

	var transacoes, lancamentos int64
	require.NoError(t, a.db.Session(a.ctx).Raw(
		"SELECT count(*) FROM wager_transactions").Scan(&transacoes).Error)
	require.NoError(t, a.db.Session(a.ctx).Raw(
		"SELECT count(*) FROM wallet_ledger_entries").Scan(&lancamentos).Error)
	assert.Zero(t, transacoes)
	assert.Zero(t, lancamentos)

	// E a mesma abertura passa a funcionar depois — o desfazimento não deixou
	// a conexão nem o índice num estado que impeça a próxima tentativa.
	_, err = a.opener.Open(a.ctx, appwallet.OpenInput{
		PlayerID: jogador, InitialBalance: valor(t, "100.00"),
	})
	require.NoError(t, err)
}

// Os repositórios de escrita exigem transação aberta. Sem isso, um lançamento
// seria confirmado sozinho e o ledger passaria a discordar da carteira.
func TestEscritaFinanceiraForaDeTransacaoEhRecusada(t *testing.T) {
	a := novoAmbiente(t)

	err := postgres.NewLedgerRepository(a.db).Append(a.ctx, lancamentoQualquer(t))
	require.Error(t, err)
	assert.ErrorIs(t, err, postgres.ErrNoTransaction)
}

// outboxQueFalha substitui a outbox por uma que sempre recusa, para exercitar o
// desfazimento no meio da abertura.
type outboxQueFalha struct{ err error }

func (o outboxQueFalha) Enqueue(context.Context, event.Envelope) error { return o.err }

// lancamentoQualquer monta um lançamento válido, para exercitar a exigência de
// transação aberta sem depender de estado no banco.
func lancamentoQualquer(t *testing.T) ledger.Entry {
	t.Helper()
	lid, err := ledger.NewID()
	require.NoError(t, err)
	wid, err := domainwallet.NewID()
	require.NoError(t, err)
	tid, err := wagering.NewTransactionID()
	require.NoError(t, err)

	e, err := ledger.New(lid, wid, tid, domainwallet.Credit,
		valor(t, "10.00"), valor(t, "0.00"), valor(t, "10.00"), time.Now().UTC())
	require.NoError(t, err)
	return e
}
