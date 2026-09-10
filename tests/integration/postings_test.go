//go:build integration

package integration_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	appledger "github.com/Lauiskk/munchkin/internal/app/ledger"
	appwallet "github.com/Lauiskk/munchkin/internal/app/wallet"
	domain "github.com/Lauiskk/munchkin/internal/domain/wagering"
	domainwallet "github.com/Lauiskk/munchkin/internal/domain/wallet"
)

// partida é uma linha de ledger_postings, como o banco a guarda.
type partida struct {
	Kind        string `gorm:"column:account_kind"`
	WalletID    *uuid.UUID
	Currency    string
	AmountMinor int64
}

// partidasDe devolve as partidas de uma transação, carteira antes da casa.
func (a ambienteExtrato) partidasDe(t *testing.T, transacao uuid.UUID) []partida {
	t.Helper()
	var linhas []partida
	require.NoError(t, a.db.Session(a.ctx).Raw(`
		SELECT account_kind, wallet_id, currency, amount_minor
		  FROM ledger_postings WHERE transaction_id = ?
		 ORDER BY account_kind DESC`, transacao).Scan(&linhas).Error)
	return linhas
}

// AC-1: um débito de 80,00 vira -80,00 na carteira e +80,00 na casa. É a
// contrapartida que transforma partida simples em partida dobrada.
func TestApostaProduzParDePartidasQueSeAnulam(t *testing.T) {
	a := novoAmbienteExtrato(t)
	w, p := a.carteiraCom(t, "100.00")

	out, err := a.processor.Process(a.ctx, operacao(t, w, p, "bet-partidas", domain.Bet, "80.00"))
	require.NoError(t, err)
	require.Equal(t, domain.Processed, out.Status)

	linhas := a.partidasDe(t, uuid.UUID(out.TransactionID))
	require.Len(t, linhas, 2, "todo lançamento tem exatamente duas partidas")

	assert.Equal(t, "WALLET", linhas[0].Kind)
	require.NotNil(t, linhas[0].WalletID)
	assert.Equal(t, uuid.UUID(w), *linhas[0].WalletID)
	assert.Equal(t, int64(-8000), linhas[0].AmountMinor)

	assert.Equal(t, "HOUSE", linhas[1].Kind)
	assert.Nil(t, linhas[1].WalletID, "a casa não pertence a carteira nenhuma")
	assert.Equal(t, int64(8000), linhas[1].AmountMinor)

	assert.Zero(t, linhas[0].AmountMinor+linhas[1].AmountMinor, "os livros fecham")
}

// AC-2: a abertura credita a carteira, e quem paga é a casa.
func TestAberturaProduzParDePartidas(t *testing.T) {
	a := novoAmbienteExtrato(t)

	out, err := a.opener.Open(a.ctx, appwallet.OpenInput{
		PlayerID: jogadorNovo(t), InitialBalance: valor(t, "1000.00"),
	})
	require.NoError(t, err)

	var soma, linhas int64
	require.NoError(t, a.db.Session(a.ctx).Raw(`
		SELECT COALESCE(SUM(p.amount_minor), 0), COUNT(*)
		  FROM ledger_postings p
		  JOIN wager_transactions t ON t.id = p.transaction_id
		 WHERE t.wallet_id = ?`, uuid.UUID(out.Wallet.ID())).Row().Scan(&soma, &linhas))

	assert.Equal(t, int64(2), linhas)
	assert.Zero(t, soma)

	var carteira int64
	require.NoError(t, a.db.Session(a.ctx).Raw(`
		SELECT amount_minor FROM ledger_postings
		 WHERE account_kind = 'WALLET' AND wallet_id = ?`,
		uuid.UUID(out.Wallet.ID())).Scan(&carteira).Error)
	assert.Equal(t, int64(100000), carteira, "abrir com saldo credita a carteira")
}

// AC-3: LOSS não movimenta, logo não tem lançamento — e sem lançamento não há
// projeção. Uma recusa por saldo, pelo mesmo motivo, também não gera partida.
func TestOperacaoSemMovimentacaoNaoGeraPartida(t *testing.T) {
	a := novoAmbienteExtrato(t)
	w, p := a.carteiraCom(t, "50.00")

	perda, err := a.processor.Process(a.ctx, operacao(t, w, p, "loss-1", domain.Loss, "0.00"))
	require.NoError(t, err)
	require.Equal(t, domain.Processed, perda.Status)
	assert.Empty(t, a.partidasDe(t, uuid.UUID(perda.TransactionID)))

	recusada, err := a.processor.Process(a.ctx, operacao(t, w, p, "bet-alta", domain.Bet, "500.00"))
	require.NoError(t, err)
	require.Equal(t, domain.Rejected, recusada.Status)
	assert.Empty(t, a.partidasDe(t, uuid.UUID(recusada.TransactionID)))
}

// AC-9: o replay não duplica a contabilidade, pelo mesmo motivo que não duplica
// o lançamento — a operação é a mesma, e ela já tem desfecho.
func TestReplayNaoDuplicaPartidas(t *testing.T) {
	a := novoAmbienteExtrato(t)
	w, p := a.carteiraCom(t, "100.00")

	entrada := operacao(t, w, p, "bet-replay", domain.Bet, "10.00")
	primeira, err := a.processor.Process(a.ctx, entrada)
	require.NoError(t, err)

	for range 3 {
		repetida, err := a.processor.Process(a.ctx, entrada)
		require.NoError(t, err)
		require.Equal(t, primeira.TransactionID, repetida.TransactionID)
	}

	assert.Len(t, a.partidasDe(t, uuid.UUID(primeira.TransactionID)), 2)
}

// AC-4, AC-5, AC-E1, AC-E2: as invariantes que o BANCO carrega. Cada caso é uma
// escrita que a aplicação nunca tenta — e que, se tentasse, seria recusada.
func TestBancoRecusaContabilidadeQueNaoFecha(t *testing.T) {
	dbCfg := migrado(t)
	a := montarAmbienteExtrato(t, novoAmbienteSobre(t, dbCfg))
	dono := abrir(t, asOwner(dbCfg))
	w, p := a.carteiraCom(t, "100.00")

	comPartidas, err := a.processor.Process(a.ctx, operacao(t, w, p, "bet-base", domain.Bet, "10.00"))
	require.NoError(t, err)
	require.Equal(t, domain.Processed, comPartidas.Status)

	// Uma transação que EXISTE e não tem partida nenhuma. LOSS é processada e
	// não movimenta: sem lançamento, sem projeção. Serve de terreno limpo para
	// exercitar o gatilho — e a chave estrangeira exige uma transação real.
	semPartidas := func(t *testing.T, externo string) uuid.UUID {
		t.Helper()
		out, err := a.processor.Process(a.ctx, operacao(t, w, p, externo, domain.Loss, "0.00"))
		require.NoError(t, err)
		require.Empty(t, a.partidasDe(t, uuid.UUID(out.TransactionID)))
		return uuid.UUID(out.TransactionID)
	}

	inserir := func(transacao uuid.UUID, kind string, carteira any, moeda string, valor int64) error {
		return a.db.Session(a.ctx).Exec(`
			INSERT INTO ledger_postings
			  (id, transaction_id, account_kind, wallet_id, currency, amount_minor, created_at)
			VALUES (?, ?, ?, ?, ?, ?, now())`,
			uuid.New(), transacao, kind, carteira, moeda, valor).Error
	}

	// AC-4: a metade solitária só é descoberta no COMMIT. É por isso que o
	// gatilho é diferido: dentro da transação, uma partida sozinha é um estado
	// legítimo de meio de caminho — todo par correto passa por ele.
	t.Run("partida sozinha não confirma", func(t *testing.T) {
		err := inserir(semPartidas(t, "sozinha"), "WALLET", uuid.UUID(w), "BRL", -100)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "solitaria")
	})

	t.Run("par que não soma zero não confirma", func(t *testing.T) {
		transacao := semPartidas(t, "sobra")
		err := a.db.Session(a.ctx).Exec(`
			INSERT INTO ledger_postings
			  (id, transaction_id, account_kind, wallet_id, currency, amount_minor, created_at)
			VALUES (?, ?, 'WALLET', ?, 'BRL', -100, now()),
			       (?, ?, 'HOUSE',  NULL, 'BRL',  90, now())`,
			uuid.New(), transacao, uuid.UUID(w), uuid.New(), transacao).Error
		require.Error(t, err)
		assert.Contains(t, err.Error(), "nao fecham")
	})

	// AC-E1: duas moedas somariam zero em unidades mínimas e mentiriam. O
	// gatilho conta as moedas antes de somar os números.
	t.Run("par em moedas diferentes não confirma", func(t *testing.T) {
		transacao := semPartidas(t, "moedas")
		err := a.db.Session(a.ctx).Exec(`
			INSERT INTO ledger_postings
			  (id, transaction_id, account_kind, wallet_id, currency, amount_minor, created_at)
			VALUES (?, ?, 'WALLET', ?, 'BRL', -100, now()),
			       (?, ?, 'HOUSE',  NULL, 'USD',  100, now())`,
			uuid.New(), transacao, uuid.UUID(w), uuid.New(), transacao).Error
		require.Error(t, err)
		assert.Contains(t, err.Error(), "moedas")
	})

	// AC-E2: partida de valor zero não é lançamento, é ruído.
	t.Run("partida de valor zero é recusada", func(t *testing.T) {
		err := inserir(semPartidas(t, "zero"), "HOUSE", nil, "BRL", 0)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ledger_postings_amount_not_zero")
	})

	// A conta da casa não pertence a carteira nenhuma, e a da carteira não
	// existe sem ela. A linha meio preenchida não passa.
	t.Run("conta da casa com carteira é recusada", func(t *testing.T) {
		err := inserir(semPartidas(t, "casa-com-carteira"), "HOUSE", uuid.UUID(w), "BRL", 100)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ledger_postings_account_shape")
	})

	t.Run("conta de carteira sem carteira é recusada", func(t *testing.T) {
		err := inserir(semPartidas(t, "carteira-sem-carteira"), "WALLET", nil, "BRL", 100)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ledger_postings_account_shape")
	})

	// AC-5: append-only nas MESMAS duas camadas do ledger. A do privilégio não
	// alcança o dono do schema; a do gatilho alcança.
	t.Run("a aplicação não tem privilégio de alterar nem apagar", func(t *testing.T) {
		err := a.db.Session(a.ctx).Exec(`UPDATE ledger_postings SET amount_minor = 1`).Error
		require.Error(t, err)
		assert.Contains(t, err.Error(), "permission denied")

		err = a.db.Session(a.ctx).Exec(`DELETE FROM ledger_postings`).Error
		require.Error(t, err)
		assert.Contains(t, err.Error(), "permission denied")
	})

	t.Run("nem o dono do schema consegue alterar", func(t *testing.T) {
		ctx := context.Background()

		err := dono.Session(ctx).Exec(`UPDATE ledger_postings SET amount_minor = 1`).Error
		require.Error(t, err)
		assert.Contains(t, err.Error(), "append-only")

		err = dono.Session(ctx).Exec(`DELETE FROM ledger_postings`).Error
		require.Error(t, err)
		assert.Contains(t, err.Error(), "append-only")

		err = dono.Session(ctx).Exec(`TRUNCATE ledger_postings`).Error
		require.Error(t, err, "TRUNCATE não passa por UPDATE nem DELETE e precisa de gatilho próprio")
		assert.Contains(t, err.Error(), "append-only")
	})

	// E a tabela continua aceitando escrita: append-only não é somente leitura.
	assert.Len(t, a.partidasDe(t, uuid.UUID(comPartidas.TransactionID)), 2)
}

// AC-7: o balancete fecha, e a conta da casa é o simétrico da soma das
// carteiras. É a conferência que a reconciliação por carteira não faz: ela
// olha uma carteira de cada vez, e esta olha a plataforma inteira.
func TestBalanceteFechaEEspelhaAsCarteiras(t *testing.T) {
	a := novoAmbienteExtrato(t)
	w, p := a.carteiraCom(t, "100.00")

	for _, caso := range []struct {
		externo string
		kind    domain.Kind
		valor   string
	}{
		{"bal-bet", domain.Bet, "30.00"},
		{"bal-win", domain.Win, "12.00"},
		{"bal-loss", domain.Loss, "0.00"},
	} {
		out, err := a.processor.Process(a.ctx, operacao(t, w, p, caso.externo, caso.kind, caso.valor))
		require.NoError(t, err)
		require.Equal(t, domain.Processed, out.Status)
	}

	balancete, err := a.balancer.Run(a.ctx)
	require.NoError(t, err)

	assert.True(t, balancete.Balanced)
	assert.Zero(t, balancete.UnbalancedTransactions)
	require.Len(t, balancete.Currencies, 1)

	moeda := balancete.Currencies[0]
	assert.Equal(t, "BRL", moeda.Currency.String())
	assert.Equal(t, "0.00", moeda.Total.Amount(), "a soma de todas as contas é zero")
	require.Len(t, moeda.Accounts, 2)

	porTipo := map[string]string{}
	for _, c := range moeda.Accounts {
		porTipo[string(c.Kind)] = c.Balance.Amount()
	}
	// Abertura 100, aposta -30, ganho +12 = 82,00 na carteira.
	assert.Equal(t, "82.00", porTipo["WALLET"])
	assert.Equal(t, "-82.00", porTipo["HOUSE"], "a casa é o simétrico das carteiras")

	// E o balancete concorda com o saldo armazenado — a conferência global que
	// a reconciliação por carteira, sozinha, não alcança.
	var saldoTotal int64
	require.NoError(t, a.db.Session(a.ctx).
		Raw(`SELECT COALESCE(SUM(balance_minor), 0) FROM wallets`).Scan(&saldoTotal).Error)
	assert.Equal(t, int64(8200), saldoTotal)
}

// AC-6: o preenchimento do histórico. Um invariante que só vale do deploy em
// diante deixaria metade dos dados fora dele, e o balancete de uma base já em
// uso nasceria errado.
func TestMigrationPreencheHistoricoExistente(t *testing.T) {
	dbCfg := migrado(t)
	a := montarAmbienteExtrato(t, novoAmbienteSobre(t, dbCfg))
	w, p := a.carteiraCom(t, "100.00")

	out, err := a.processor.Process(a.ctx, operacao(t, w, p, "historico", domain.Bet, "25.00"))
	require.NoError(t, err)
	transacao := uuid.UUID(out.TransactionID)

	// Volta para antes das partidas dobradas: a tabela some, o ledger fica.
	m := novoMigrator(t, dbCfg)
	require.NoError(t, m.Down())

	dono := abrir(t, asOwner(dbCfg))
	var lancamentos int64
	require.NoError(t, dono.Session(context.Background()).
		Raw(`SELECT count(*) FROM wallet_ledger_entries`).Scan(&lancamentos).Error)
	require.Positive(t, lancamentos, "o ledger do §6.4 não é tocado pela reversão")

	// E aplica de novo: a contabilidade é reconstruída a partir do ledger.
	require.NoError(t, m.Up())

	linhas := a.partidasDe(t, transacao)
	require.Len(t, linhas, 2, "a história entra junto, não só o que vier depois")
	assert.Zero(t, linhas[0].AmountMinor+linhas[1].AmountMinor)

	balancete, err := a.balancer.Run(a.ctx)
	require.NoError(t, err)
	assert.True(t, balancete.Balanced)
	assert.Equal(t, int64(2*lancamentos), balancete.CheckedPostings,
		"duas partidas por lançamento existente")
}

// carteiraCom abre uma carteira com o saldo pedido.
func (a ambienteExtrato) carteiraCom(t *testing.T, saldo string) (domainwallet.ID, domainwallet.PlayerID) {
	t.Helper()
	jogador := jogadorNovo(t)
	out, err := a.opener.Open(a.ctx, appwallet.OpenInput{
		PlayerID: jogador, InitialBalance: valor(t, saldo),
	})
	require.NoError(t, err)
	return out.Wallet.ID(), jogador
}

// TestBalanceteDizPorExtensoQuandoOTotalNaoCabe guarda o desfecho de um total
// que estoura o int64.
//
// Achado numa passada de QA: com duas carteiras perto do teto, o balancete
// respondia 500 "erro inesperado", e o log trazia só o erro de Scan do driver.
// A causa é que SUM(bigint) no PostgreSQL devolve numeric — que não estoura —,
// e o estouro acontece do lado de Go.
//
// O que o teste fixa: o limite do Money vale por VALOR e NÃO COMPÕE para o
// agregado. Somar todas as carteiras de uma moeda pode passar do teto sem que
// nenhuma delas tenha passado. É limite declarado, e a mensagem tem de nomear a
// conta e a moeda para que quem opera saiba o que aconteceu.
func TestBalanceteDizPorExtensoQuandoOTotalNaoCabe(t *testing.T) {
	a := novoAmbienteExtrato(t)
	const teto = "92233720368547758.07"

	// Duas carteiras no teto: cada uma cabe, a soma das duas não.
	a.carteiraCom(t, teto)
	a.carteiraCom(t, teto)

	_, err := a.balancer.Run(a.ctx)

	require.Error(t, err, "o total não cabe: o relatório tem de dizer isso")
	require.ErrorIs(t, err, appledger.ErrTotalNaoRepresentavel)
	// A consulta ordena por (moeda, tipo de conta), então a conta da casa é a
	// primeira a estourar — e é ela que a mensagem nomeia.
	assert.Contains(t, err.Error(), "HOUSE")
	assert.Contains(t, err.Error(), "BRL", "a mensagem nomeia a conta e a moeda")
	assert.NotContains(t, err.Error(), "Scan error",
		"o erro do driver não pode vazar como se fosse a explicação")
}
