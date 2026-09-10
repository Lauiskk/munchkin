package ledger_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/internal/domain/ledger"
	"github.com/Lauiskk/munchkin/internal/domain/money"
	"github.com/Lauiskk/munchkin/internal/domain/wallet"
)

// AC-1: um débito de 80,00 na carteira tem de virar -80,00 na conta dela e
// +80,00 na casa. O sinal é o que faz a soma dar zero.
func TestDebitoProjetaParQueSeAnula(t *testing.T) {
	lid, wid, tid := chaves(t)

	e, err := ledger.New(lid, wid, tid, wallet.Debit,
		brl(t, "80.00"), brl(t, "100.00"), brl(t, "20.00"), agora)
	require.NoError(t, err)

	par, err := ledger.DoubleEntry(e)
	require.NoError(t, err)

	carteira, casa := par[0], par[1]
	assert.Equal(t, ledger.WalletAccount, carteira.Account().Kind())
	assert.Equal(t, wid, carteira.Account().WalletID())
	assert.Equal(t, "-80.00", carteira.Amount().Amount())

	assert.Equal(t, ledger.HouseAccount, casa.Account().Kind())
	assert.True(t, casa.Account().WalletID().IsZero(),
		"a conta da casa não pertence a carteira nenhuma")
	assert.Equal(t, "80.00", casa.Amount().Amount())
	assert.True(t, casa.Amount().IsPositive())

	require.NoError(t, ledger.Balanced(par[:]))
}

// AC-2: a abertura credita a carteira, e quem paga é a casa.
func TestCreditoProjetaParQueSeAnula(t *testing.T) {
	lid, wid, tid := chaves(t)

	e, err := ledger.New(lid, wid, tid, wallet.Credit,
		brl(t, "100.00"), brl(t, "0.00"), brl(t, "100.00"), agora)
	require.NoError(t, err)

	par, err := ledger.DoubleEntry(e)
	require.NoError(t, err)

	assert.Equal(t, "100.00", par[0].Amount().Amount())
	assert.Equal(t, "-100.00", par[1].Amount().Amount())
	require.NoError(t, ledger.Balanced(par[:]))
}

// As duas partidas descrevem o MESMO fato: mesma transação, mesmo instante.
// Datá-las em momentos diferentes faria o extrato contar duas histórias.
func TestParCompartilhaTransacaoEInstante(t *testing.T) {
	lid, wid, tid := chaves(t)

	e, err := ledger.New(lid, wid, tid, wallet.Debit,
		brl(t, "10.00"), brl(t, "30.00"), brl(t, "20.00"), agora)
	require.NoError(t, err)

	par, err := ledger.DoubleEntry(e)
	require.NoError(t, err)

	assert.Equal(t, tid, par[0].TransactionID())
	assert.Equal(t, tid, par[1].TransactionID())
	assert.Equal(t, e.CreatedAt(), par[0].CreatedAt())
	assert.Equal(t, e.CreatedAt(), par[1].CreatedAt())
	assert.NotEqual(t, par[0].ID(), par[1].ID(),
		"cada partida é uma linha própria")
}

// AC-E2: partida de valor zero não é lançamento, é ruído. E é o mesmo motivo
// pelo qual LOSS não gera partida nenhuma: não houve movimentação.
func TestPartidaDeValorZeroEhRecusada(t *testing.T) {
	_, wid, tid := chaves(t)

	conta, err := ledger.WalletAccountOf(wid, money.BRL)
	require.NoError(t, err)

	id, err := ledger.NewPostingID()
	require.NoError(t, err)

	_, err = ledger.NewPosting(id, tid, conta, brl(t, "0.00"), agora)
	assert.ErrorIs(t, err, ledger.ErrZeroPosting)
}

// A partida tem de estar na moeda da própria conta. Sem isso, um par em moedas
// distintas somaria zero em unidades mínimas e mentiria.
func TestPartidaEmMoedaDiferenteDaContaEhRecusada(t *testing.T) {
	_, wid, tid := chaves(t)

	conta, err := ledger.WalletAccountOf(wid, money.BRL)
	require.NoError(t, err)
	usd, err := money.Parse("10.00", money.USD)
	require.NoError(t, err)

	id, err := ledger.NewPostingID()
	require.NoError(t, err)

	_, err = ledger.NewPosting(id, tid, conta, usd, agora)
	assert.ErrorIs(t, err, ledger.ErrCurrencyMismatch)
}

// Balanced é a regra escrita uma vez e cobrada dos dois lados: de quem escreve
// o par e de quem o lê de volta do banco.
func TestBalancedRecusaConjuntoQueNaoFecha(t *testing.T) {
	_, wid, tid := chaves(t)

	contaCarteira, err := ledger.WalletAccountOf(wid, money.BRL)
	require.NoError(t, err)
	contaCasa, err := ledger.HouseAccountOf(money.BRL)
	require.NoError(t, err)

	partida := func(c ledger.Account, valor string) ledger.Posting {
		id, err := ledger.NewPostingID()
		require.NoError(t, err)
		p, err := ledger.NewPosting(id, tid, c, brl(t, valor), agora)
		require.NoError(t, err)
		return p
	}

	t.Run("sobra", func(t *testing.T) {
		err := ledger.Balanced([]ledger.Posting{
			partida(contaCarteira, "-80.00"),
			partida(contaCasa, "70.00"),
		})
		assert.ErrorIs(t, err, ledger.ErrUnbalanced)
	})

	t.Run("partida sozinha", func(t *testing.T) {
		err := ledger.Balanced([]ledger.Posting{partida(contaCarteira, "-80.00")})
		assert.ErrorIs(t, err, ledger.ErrUnbalanced)
	})

	t.Run("par correto", func(t *testing.T) {
		assert.NoError(t, ledger.Balanced([]ledger.Posting{
			partida(contaCarteira, "-80.00"),
			partida(contaCasa, "80.00"),
		}))
	})
}

// A conta da casa é uma por MOEDA. Duas moedas, duas casas — dinheiro de moedas
// diferentes não se soma.
func TestContaDaCasaEhUmaPorMoeda(t *testing.T) {
	brlCasa, err := ledger.HouseAccountOf(money.BRL)
	require.NoError(t, err)
	usdCasa, err := ledger.HouseAccountOf(money.USD)
	require.NoError(t, err)

	assert.NotEqual(t, brlCasa, usdCasa)
	assert.Equal(t, "HOUSE:BRL", brlCasa.String())
}
