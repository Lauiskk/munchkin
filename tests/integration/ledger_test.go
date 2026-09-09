//go:build integration

package integration_test

import (
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/internal/adapter/postgres"
	"github.com/Lauiskk/munchkin/internal/app"
	appwagering "github.com/Lauiskk/munchkin/internal/app/wagering"
	appwallet "github.com/Lauiskk/munchkin/internal/app/wallet"
	domain "github.com/Lauiskk/munchkin/internal/domain/wagering"
	domainwallet "github.com/Lauiskk/munchkin/internal/domain/wallet"
)

type ambienteExtrato struct {
	ambienteOperacao
	statement  *appwallet.Statement
	reconciler *appwallet.Reconciler
	relogio    *relogioMovel
}

func novoAmbienteExtrato(t *testing.T) ambienteExtrato {
	t.Helper()
	a := novoAmbiente(t)
	rel := &relogioMovel{agora: agoraFixo}
	metricas := &registroDeMetricas{}
	transacoes := postgres.NewTransactionRepository(a.db)
	carteiras := postgres.NewWalletRepository(a.db)
	entradas := postgres.NewLedgerRepository(a.db)

	return ambienteExtrato{
		ambienteOperacao: ambienteOperacao{
			ambiente: a,
			processor: appwagering.NewProcessor(a.db, carteiras, transacoes, entradas,
				postgres.NewOutboxRepository(a.db), rel, metricas),
			querier:  appwagering.NewQuerier(transacoes),
			metricas: metricas,
		},
		statement: appwallet.NewStatement(carteiras, entradas),
		reconciler: appwallet.NewReconciler(carteiras, metricas,
			slog.New(slog.NewTextHandler(io.Discard, nil))),
		relogio: rel,
	}
}

// apostas cria n apostas de 1.00, cada uma um instante depois da anterior.
func (a ambienteExtrato) apostas(t *testing.T, w domainwallet.ID, p domainwallet.PlayerID, n int) {
	t.Helper()
	for i := range n {
		a.relogio.avancar(time.Second)
		out, err := a.processor.Process(a.ctx,
			operacao(t, w, p, fmt.Sprintf("tx-%d", i), domain.Bet, "1.00"))
		require.NoError(t, err)
		require.Equal(t, domain.Processed, out.Status)
	}
}

// paginar percorre o extrato inteiro e devolve os identificadores na ordem.
func (a ambienteExtrato) paginar(t *testing.T, w domainwallet.ID, limite int) []string {
	t.Helper()
	var vistos []string
	var depois *appwallet.Position

	for volta := 0; ; volta++ {
		require.Less(t, volta, 100, "paginação não terminou — provável laço")

		pagina, err := a.statement.List(a.ctx, w, depois, limite)
		require.NoError(t, err)
		for _, e := range pagina.Entries {
			vistos = append(vistos, e.ID().String())
		}
		if pagina.Next == nil {
			return vistos
		}
		require.Len(t, pagina.Entries, limite, "só a última página pode vir incompleta")
		depois = pagina.Next
	}
}

// AC-1, AC-2, AC-3
func TestExtratoPaginaSemRepetirNemPular(t *testing.T) {
	a := novoAmbienteExtrato(t)
	w, p := a.carteira(t, "100.00")
	a.apostas(t, w, p, 7) // mais a abertura: 8 lançamentos

	vistos := a.paginar(t, w, 3)

	assert.Len(t, vistos, 8)
	assert.Len(t, unicos(vistos), 8, "nenhum lançamento pode aparecer duas vezes")

	// A ordem é do mais recente para o mais antigo, e a abertura é a mais
	// antiga: ela tem de ser a última.
	assert.Equal(t, int64(1), a.conta(t, `SELECT count(*) FROM wallet_ledger_entries
		WHERE id = ?::uuid AND transaction_id IN
		      (SELECT id FROM wager_transactions WHERE kind = 'OPENING')`,
		vistos[len(vistos)-1]))
}

// AC-E4 — todos os lançamentos no MESMO instante.
//
// É o caso que quebra uma paginação por keyset mal feita, e não é hipotético:
// duas operações no mesmo milissegundo acontecem. Sem o desempate por
// identificador, a página seguinte repetiria ou pularia lançamentos.
func TestExtratoComLancamentosNoMesmoInstante(t *testing.T) {
	a := novoAmbienteExtrato(t)
	w, p := a.carteira(t, "100.00")

	// Relógio parado: os seis lançamentos caem no mesmo created_at.
	for i := range 5 {
		out, err := a.processor.Process(a.ctx,
			operacao(t, w, p, fmt.Sprintf("tx-%d", i), domain.Bet, "1.00"))
		require.NoError(t, err)
		require.Equal(t, domain.Processed, out.Status)
	}

	vistos := a.paginar(t, w, 2)

	assert.Len(t, vistos, 6)
	assert.Len(t, unicos(vistos), 6)
	assert.Equal(t, int64(1), a.conta(t, `SELECT count(DISTINCT created_at)
		FROM wallet_ledger_entries WHERE wallet_id = ?`, uuid.UUID(w)),
		"o teste só vale se os instantes forem mesmo iguais")
}

// AC-4 — um lançamento novo durante a paginação não desloca o que vem depois.
func TestLancamentoNovoNaoDeslocaAsPaginasSeguintes(t *testing.T) {
	a := novoAmbienteExtrato(t)
	w, p := a.carteira(t, "100.00")
	a.apostas(t, w, p, 5) // 6 lançamentos

	primeira, err := a.statement.List(a.ctx, w, nil, 2)
	require.NoError(t, err)
	require.NotNil(t, primeira.Next)

	// Chega uma aposta nova, mais recente que tudo que já foi visto.
	a.apostas(t, w, p, 1)

	restante := []string{}
	depois := primeira.Next
	for depois != nil {
		pagina, err := a.statement.List(a.ctx, w, depois, 2)
		require.NoError(t, err)
		for _, e := range pagina.Entries {
			restante = append(restante, e.ID().String())
		}
		depois = pagina.Next
	}

	assert.Len(t, restante, 4, "as páginas seguintes continuam com os 4 restantes")
	for _, e := range primeira.Entries {
		assert.NotContains(t, restante, e.ID().String(), "nada pode repetir")
	}
}

// AC-6
func TestLimiteAcimaDoTetoEhRecusado(t *testing.T) {
	a := novoAmbienteExtrato(t)
	w, _ := a.carteira(t, "100.00")

	_, err := a.statement.List(a.ctx, w, nil, appwallet.LedgerPageMax+1)

	require.Error(t, err, "sem teto, um limite grande vira um jeito barato de esgotar memória")
}

// AC-7
func TestCarteiraInexistenteNoExtratoENaReconciliacao(t *testing.T) {
	a := novoAmbienteExtrato(t)
	inexistente, err := domainwallet.NewID()
	require.NoError(t, err)

	_, err = a.statement.List(a.ctx, inexistente, nil, 10)
	assert.ErrorIs(t, err, app.ErrNotFound,
		"página vazia esconderia a diferença entre carteira sem movimento e carteira que não existe")

	_, err = a.reconciler.Run(a.ctx, inexistente)
	assert.ErrorIs(t, err, app.ErrNotFound)
}

// AC-9 e AC-10
func TestReconciliacaoConfereSemAlterarNada(t *testing.T) {
	a := novoAmbienteExtrato(t)
	w, p := a.carteira(t, "100.00")
	a.apostas(t, w, p, 3)

	antes := a.instantaneo(t, w)

	r, err := a.reconciler.Run(a.ctx, w)

	require.NoError(t, err)
	assert.True(t, r.Consistent)
	assert.Equal(t, "97.00 BRL", r.Stored.String())
	assert.Equal(t, "97.00 BRL", r.Calculated.String())
	assert.Equal(t, "0.00 BRL", r.Difference.String())
	assert.Equal(t, int64(4), r.CheckedEntries)
	assert.Equal(t, antes, a.instantaneo(t, w), "a conferência não pode escrever nada")
}

// AC-11 e AC-E5 — divergência forçada, nos dois sentidos.
func TestDivergenciaEhReportadaComSinal(t *testing.T) {
	casos := map[string]struct {
		saldoForcado int64
		diferenca    string
	}{
		"saldo maior que o ledger": {10000, "3.00 BRL"},
		"saldo menor que o ledger": {9000, "-7.00 BRL"},
	}

	for nome, c := range casos {
		t.Run(nome, func(t *testing.T) {
			a := novoAmbienteExtrato(t)
			w, p := a.carteira(t, "100.00")
			a.apostas(t, w, p, 3) // ledger soma 97.00

			// Corrompe o saldo por fora, como um defeito faria.
			require.NoError(t, a.db.Session(a.ctx).Exec(
				`UPDATE wallets SET balance_minor = ? WHERE id = ?`,
				c.saldoForcado, uuid.UUID(w)).Error)

			r, err := a.reconciler.Run(a.ctx, w)

			require.NoError(t, err, "divergência é resultado, não erro")
			assert.False(t, r.Consistent)
			assert.Equal(t, c.diferenca, r.Difference.String())
			assert.Equal(t, "97.00 BRL", r.Calculated.String(),
				"o ledger é a fonte, e ele não foi tocado")
		})
	}
}

// AC-E1
func TestCarteiraSemMovimentoTemExtratoVazioEReconciliaConsistente(t *testing.T) {
	a := novoAmbienteExtrato(t)
	w, _ := a.carteira(t, "0.00") // saldo zero não cria OPENING nem lançamento

	pagina, err := a.statement.List(a.ctx, w, nil, 10)
	require.NoError(t, err)
	assert.Empty(t, pagina.Entries)
	assert.Nil(t, pagina.Next)

	r, err := a.reconciler.Run(a.ctx, w)
	require.NoError(t, err)
	assert.True(t, r.Consistent)
	assert.Zero(t, r.CheckedEntries)
	assert.Equal(t, "0.00 BRL", r.Difference.String())
}

// AC-E3
func TestCursorAlemDoFimDevolvePaginaVazia(t *testing.T) {
	a := novoAmbienteExtrato(t)
	w, p := a.carteira(t, "100.00")
	a.apostas(t, w, p, 2)

	// Uma posição anterior a tudo: em ordem decrescente, não sobra nada depois.
	pagina, err := a.statement.List(a.ctx, w, &appwallet.Position{
		CreatedAt: agoraFixo.Add(-time.Hour), ID: uuid.Nil,
	}, 10)

	require.NoError(t, err, "posição sem resultados é página vazia, não erro")
	assert.Empty(t, pagina.Entries)
	assert.Nil(t, pagina.Next)
}

// instantaneoPorUUID captura o estado financeiro de uma carteira pelo seu UUID.
func (a ambienteOperacao) instantaneoPorUUID(t *testing.T, id uuid.UUID) [3]int64 {
	t.Helper()
	return [3]int64{
		a.conta(t, `SELECT balance_minor FROM wallets WHERE id = ?`, id),
		a.conta(t, `SELECT version FROM wallets WHERE id = ?`, id),
		a.conta(t, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = ?`, id),
	}
}

// instantaneo captura o que a reconciliação não pode alterar.
func (a ambienteExtrato) instantaneo(t *testing.T, w domainwallet.ID) [3]int64 {
	t.Helper()
	return [3]int64{
		a.conta(t, `SELECT balance_minor FROM wallets WHERE id = ?`, uuid.UUID(w)),
		a.conta(t, `SELECT version FROM wallets WHERE id = ?`, uuid.UUID(w)),
		a.conta(t, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = ?`, uuid.UUID(w)),
	}
}

func unicos(s []string) []string {
	visto := map[string]struct{}{}
	out := make([]string, 0, len(s))
	for _, v := range s {
		if _, ok := visto[v]; ok {
			continue
		}
		visto[v] = struct{}{}
		out = append(out, v)
	}
	return out
}
