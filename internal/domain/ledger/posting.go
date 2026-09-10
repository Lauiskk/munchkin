package ledger

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Lauiskk/munchkin/internal/domain/money"
	"github.com/Lauiskk/munchkin/internal/domain/wagering"
	"github.com/Lauiskk/munchkin/internal/domain/wallet"
)

// Partidas dobradas — a contabilidade de dupla entrada, ao lado do ledger.
//
// O §6.4 especifica `wallet_ledger_entries` e diz que partidas dobradas são
// opcionais. Esta é a parte opcional, e ela é uma PROJEÇÃO: cada lançamento
// vira um par de partidas que se anulam, e nada mais escreve partidas.
//
// O desenho responde a uma pergunta só — de onde vem a contrapartida. Quando
// uma carteira perde 80,00, alguém ganha 80,00, e esse alguém é a casa. Sem a
// contrapartida não há partida dobrada; há a mesma partida simples com uma
// coluna a mais.

var (
	// ErrInvalidAccount indica conta de partida malformada.
	ErrInvalidAccount = errors.New("conta de partida inválida")
	// ErrUnbalanced indica par de partidas que não soma zero.
	ErrUnbalanced = errors.New("as partidas não se anulam")
	// ErrZeroPosting indica partida de valor zero.
	ErrZeroPosting = errors.New("partida exige valor diferente de zero")
)

// AccountKind é o tipo de conta que uma partida movimenta.
type AccountKind string

const (
	// WalletAccount é a conta de uma carteira de jogador.
	WalletAccount AccountKind = "WALLET"
	// HouseAccount é a contrapartida da casa, uma por moeda.
	//
	// Uma por MOEDA, e não uma por provedor: dinheiro de moedas diferentes não
	// se soma, e um balancete que misturasse BRL com USD não seria balancete.
	HouseAccount AccountKind = "HOUSE"
)

// Account identifica a conta movimentada por uma partida.
//
// A carteira tem identidade própria; a casa é identificada pela moeda. Guardar
// as duas num campo de texto só ("wallet:01a0…", "house:BRL") custaria a chave
// estrangeira e a checagem do banco — e é justamente o banco que precisa
// conseguir conferir isto.
type Account struct {
	kind     AccountKind
	walletID wallet.ID
	currency money.Currency
}

// WalletAccountOf devolve a conta de uma carteira.
func WalletAccountOf(id wallet.ID, c money.Currency) (Account, error) {
	if id.IsZero() {
		return Account{}, fmt.Errorf("%w: carteira ausente", ErrInvalidAccount)
	}
	if c.IsZero() {
		return Account{}, fmt.Errorf("%w: moeda ausente", ErrInvalidAccount)
	}
	return Account{kind: WalletAccount, walletID: id, currency: c}, nil
}

// HouseAccountOf devolve a conta da casa para uma moeda.
func HouseAccountOf(c money.Currency) (Account, error) {
	if c.IsZero() {
		return Account{}, fmt.Errorf("%w: moeda ausente", ErrInvalidAccount)
	}
	return Account{kind: HouseAccount, currency: c}, nil
}

// Acessores da conta.

func (a Account) Kind() AccountKind        { return a.kind }
func (a Account) WalletID() wallet.ID      { return a.walletID }
func (a Account) Currency() money.Currency { return a.currency }

// IsZero informa se a conta não foi construída.
func (a Account) IsZero() bool { return a.kind == "" }

// String devolve a conta em forma legível, para log e mensagem de erro.
func (a Account) String() string {
	if a.kind == WalletAccount {
		return "WALLET:" + a.walletID.String()
	}
	return string(a.kind) + ":" + a.currency.String()
}

// PostingID identifica uma partida.
//
// Distinto do identificador do lançamento de propósito: uma partida não é um
// lançamento, é metade de um. Reaproveitar o tipo faria duas coisas diferentes
// parecerem a mesma na assinatura das funções.
type PostingID uuid.UUID

// NewPostingID gera um identificador de partida.
func NewPostingID() (PostingID, error) {
	u, err := uuid.NewV7()
	if err != nil {
		return PostingID{}, fmt.Errorf("geração de identificador de partida: %w", err)
	}
	return PostingID(u), nil
}

// ParsePostingID converte a representação textual.
func ParsePostingID(s string) (PostingID, error) {
	u, err := uuid.Parse(s)
	if err != nil || u == uuid.Nil {
		return PostingID{}, fmt.Errorf("%w: %q", ErrInvalidID, s)
	}
	return PostingID(u), nil
}

func (i PostingID) String() string { return uuid.UUID(i).String() }
func (i PostingID) IsZero() bool   { return i == PostingID{} }

// Posting é uma partida: um valor COM SINAL numa conta.
//
// Com sinal, e não direção mais valor positivo como no lançamento do §6.4. A
// invariante desta tabela é "a soma dá zero", e uma soma sobre coluna com sinal
// é a expressão direta dessa frase — no banco vira `SUM(amount_minor) = 0`, que
// se lê igual à regra. Com direção, a mesma checagem viraria um CASE.
type Posting struct {
	id            PostingID
	transactionID wagering.TransactionID
	account       Account
	amount        money.Money
	createdAt     time.Time
}

// NewPosting constrói uma partida, validando.
//
// Usado na reconstrução a partir do banco. A criação normal é por DoubleEntry,
// que nunca produz uma partida sozinha.
func NewPosting(
	id PostingID, transactionID wagering.TransactionID,
	account Account, amount money.Money, now time.Time,
) (Posting, error) {
	if id.IsZero() {
		return Posting{}, fmt.Errorf("%w: partida sem identificador", ErrInvalidID)
	}
	if transactionID.IsZero() {
		return Posting{}, fmt.Errorf("%w: partida sem transação", ErrInvalidID)
	}
	if account.IsZero() {
		return Posting{}, fmt.Errorf("%w: ausente", ErrInvalidAccount)
	}
	if !amount.IsValid() {
		return Posting{}, money.ErrUninitialized
	}
	if amount.IsZero() {
		return Posting{}, fmt.Errorf("%w: conta %s", ErrZeroPosting, account)
	}
	if amount.Currency() != account.Currency() {
		return Posting{}, fmt.Errorf("%w: partida em %s na conta %s",
			ErrCurrencyMismatch, amount.Currency(), account)
	}
	return Posting{
		id: id, transactionID: transactionID,
		account: account, amount: amount, createdAt: now.UTC(),
	}, nil
}

// Acessores. Não há setter: a partida é imutável, como o lançamento.

func (p Posting) ID() PostingID                         { return p.id }
func (p Posting) TransactionID() wagering.TransactionID { return p.transactionID }
func (p Posting) Account() Account                      { return p.account }
func (p Posting) Amount() money.Money                   { return p.amount }
func (p Posting) CreatedAt() time.Time                  { return p.createdAt }

// DoubleEntry projeta um lançamento no par de partidas que se anulam.
//
// É a ÚNICA forma de criar partidas, e ela sempre cria as duas. É daí que vem a
// garantia de que os livros fecham: não existe caminho que escreva metade de um
// par, porque não existe função que devolva metade de um par. O gatilho diferido
// do banco confere a mesma coisa no commit — ele não está lá para o código de
// hoje, está para o caminho que alguém escrever amanhã.
//
// Os identificadores nascem aqui, em vez de virem de fora como os do lançamento.
// Ninguém referencia uma partida: ela não tem identidade no contrato, só na
// tabela — e identificador que só o banco usa é detalhe da projeção.
func DoubleEntry(e Entry) ([2]Posting, error) {
	valorCarteira, err := e.SignedAmount()
	if err != nil {
		return [2]Posting{}, err
	}
	valorCasa, err := valorCarteira.Neg()
	if err != nil {
		return [2]Posting{}, err
	}

	moeda := e.Amount().Currency()

	contaCarteira, err := WalletAccountOf(e.WalletID(), moeda)
	if err != nil {
		return [2]Posting{}, err
	}
	contaCasa, err := HouseAccountOf(moeda)
	if err != nil {
		return [2]Posting{}, err
	}

	idCarteira, err := NewPostingID()
	if err != nil {
		return [2]Posting{}, err
	}
	idCasa, err := NewPostingID()
	if err != nil {
		return [2]Posting{}, err
	}

	// O instante é o do lançamento, não o de agora: as duas linhas descrevem o
	// mesmo fato, e datá-las em momentos diferentes faria o extrato contar duas
	// histórias.
	carteira, err := NewPosting(idCarteira, e.TransactionID(), contaCarteira, valorCarteira, e.CreatedAt())
	if err != nil {
		return [2]Posting{}, err
	}
	casa, err := NewPosting(idCasa, e.TransactionID(), contaCasa, valorCasa, e.CreatedAt())
	if err != nil {
		return [2]Posting{}, err
	}

	par := [2]Posting{carteira, casa}
	if err := Balanced(par[:]); err != nil {
		return [2]Posting{}, err
	}
	return par, nil
}

// Balanced confere que um conjunto de partidas se anula.
//
// Existe separado de DoubleEntry para servir também à conferência de leitura:
// quem lê partidas do banco pode cobrar a mesma regra que quem as escreveu.
func Balanced(partidas []Posting) error {
	if len(partidas) < 2 {
		return fmt.Errorf("%w: %d partida(s)", ErrUnbalanced, len(partidas))
	}

	soma := partidas[0].Amount().ZeroOfSame()
	for _, p := range partidas {
		var err error
		if soma, err = soma.Add(p.Amount()); err != nil {
			return fmt.Errorf("%w: %w", ErrUnbalanced, err)
		}
	}
	if !soma.IsZero() {
		return fmt.Errorf("%w: sobra de %s", ErrUnbalanced, soma)
	}
	return nil
}
