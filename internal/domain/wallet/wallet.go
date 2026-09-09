// Package wallet implementa a carteira, raiz do agregado financeiro.
//
// O pacote não conhece banco, HTTP nem fila: ele conhece dinheiro e as regras
// que o saldo tem de respeitar. Quem persiste traduz; quem expõe traduz. Há um
// gate de CI que recusa dependência de infraestrutura aqui.
package wallet

import (
	"errors"
	"fmt"
	"time"

	"github.com/Lauiskk/munchkin/internal/domain/money"
)

var (
	// ErrUninitialized indica agregado que nunca passou por construtor.
	ErrUninitialized = errors.New("carteira não inicializada")
	// ErrInsufficientFunds indica débito maior que o saldo disponível.
	//
	// Tem código próprio, distinto da recusa de reversão por saldo: para quem
	// audita, "o jogador não tinha saldo para apostar" e "o dinheiro já saiu da
	// carteira, não dá para estornar" são situações diferentes.
	ErrInsufficientFunds = errors.New("saldo insuficiente")
	// ErrNonPositiveAmount indica movimentação de valor zero ou negativo.
	ErrNonPositiveAmount = errors.New("movimentação exige valor positivo")
	// ErrCurrencyMismatch indica movimentação em moeda diferente da carteira.
	ErrCurrencyMismatch = errors.New("moeda da movimentação difere da carteira")
	// ErrInvalidState indica estado persistido incoerente na reidratação.
	ErrInvalidState = errors.New("estado de carteira inválido")
)

// Direction é o sentido de uma movimentação.
type Direction string

const (
	Debit  Direction = "DEBIT"
	Credit Direction = "CREDIT"
)

// Movement descreve uma mudança de saldo já aplicada ao agregado.
//
// Carrega o saldo antes e depois porque é isso que o lançamento do ledger
// registra, e porque calcular esses valores fora do agregado abriria espaço
// para o ledger discordar da carteira.
type Movement struct {
	Direction     Direction
	Amount        money.Money
	BalanceBefore money.Money
	BalanceAfter  money.Money
	// Version é a versão da carteira DEPOIS da movimentação.
	Version    int64
	OccurredAt time.Time
}

// Wallet é a raiz do agregado financeiro.
//
// Todos os campos são não exportados. O saldo só muda pelos métodos de
// movimentação, que aplicam as invariantes — não há caminho para escrever um
// saldo direto, nem de dentro do próprio pacote de fora deste arquivo.
type Wallet struct {
	id        ID
	playerID  PlayerID
	currency  money.Currency
	balance   money.Money
	version   int64
	createdAt time.Time
	updatedAt time.Time
}

// versaoInicial é a versão de uma carteira recém-criada.
//
// O crédito de abertura faz parte da criação, não é uma movimentação posterior:
// por isso a carteira aberta com saldo positivo nasce na versão 1, e não 2.
const versaoInicial int64 = 1

// Open cria uma carteira.
//
// Devolve também a movimentação de abertura quando o saldo inicial é positivo —
// é ela que vira o lançamento de crédito no ledger. Saldo inicial zero não
// produz movimentação, e portanto não produz lançamento nem evento financeiro.
//
// Separar criação de reidratação é exigência do domínio: reidratar não pode
// reaplicar movimentação, e um construtor único com um parâmetro "é novo?"
// seria exatamente o caminho para isso acontecer por engano.
func Open(id ID, playerID PlayerID, initial money.Money, now time.Time) (*Wallet, *Movement, error) {
	if id.IsZero() {
		return nil, nil, fmt.Errorf("%w: identificador de carteira ausente", ErrInvalidID)
	}
	if playerID.IsZero() {
		return nil, nil, fmt.Errorf("%w: identificador de jogador ausente", ErrInvalidID)
	}
	if !initial.IsValid() {
		return nil, nil, fmt.Errorf("%w: saldo inicial não informado", money.ErrUninitialized)
	}
	if initial.IsNegative() {
		return nil, nil, fmt.Errorf("%w: saldo inicial negativo", ErrInvalidState)
	}

	now = now.UTC()
	w := &Wallet{
		id:        id,
		playerID:  playerID,
		currency:  initial.Currency(),
		balance:   initial,
		version:   versaoInicial,
		createdAt: now,
		updatedAt: now,
	}

	if initial.IsZero() {
		return w, nil, nil
	}

	return w, &Movement{
		Direction:     Credit,
		Amount:        initial,
		BalanceBefore: initial.ZeroOfSame(),
		BalanceAfter:  initial,
		Version:       w.version,
		OccurredAt:    now,
	}, nil
}

// Rehydrate reconstrói a carteira a partir do estado persistido.
//
// Não produz movimentação, não altera versão e não emite evento: ela apenas
// devolve ao agregado o estado que ele já tinha. Confundir reidratação com
// criação é como um saldo é creditado duas vezes.
func Rehydrate(
	id ID, playerID PlayerID, balance money.Money, version int64,
	createdAt, updatedAt time.Time,
) (*Wallet, error) {
	if id.IsZero() {
		return nil, fmt.Errorf("%w: identificador de carteira ausente", ErrInvalidID)
	}
	if playerID.IsZero() {
		return nil, fmt.Errorf("%w: identificador de jogador ausente", ErrInvalidID)
	}
	if !balance.IsValid() {
		return nil, fmt.Errorf("%w: saldo ausente", money.ErrUninitialized)
	}
	// Saldo negativo persistido significa que uma invariante do banco falhou.
	// Recusar a reidratação é preferível a operar sobre um estado que não
	// deveria existir.
	if balance.IsNegative() {
		return nil, fmt.Errorf("%w: saldo negativo persistido (%s)", ErrInvalidState, balance)
	}
	if version < versaoInicial {
		return nil, fmt.Errorf("%w: versão %d abaixo da inicial", ErrInvalidState, version)
	}

	return &Wallet{
		id:        id,
		playerID:  playerID,
		currency:  balance.Currency(),
		balance:   balance,
		version:   version,
		createdAt: createdAt.UTC(),
		updatedAt: updatedAt.UTC(),
	}, nil
}

// Debit retira valor da carteira.
//
// Recusa quando o saldo não cobre o valor. A verificação acontece antes de
// qualquer mutação, então uma recusa deixa o agregado exatamente como estava —
// o que importa porque o caso de uso ainda vai gravar a transação como recusada
// usando o saldo atual.
func (w *Wallet) Debit(amount money.Money, now time.Time) (Movement, error) {
	if err := w.checkMovement(amount); err != nil {
		return Movement{}, err
	}

	after, err := w.balance.Sub(amount)
	if err != nil {
		return Movement{}, err
	}
	if after.IsNegative() {
		return Movement{}, fmt.Errorf("%w: saldo %s, débito %s", ErrInsufficientFunds, w.balance, amount)
	}

	return w.apply(Debit, amount, after, now), nil
}

// Credit acrescenta valor à carteira.
func (w *Wallet) Credit(amount money.Money, now time.Time) (Movement, error) {
	if err := w.checkMovement(amount); err != nil {
		return Movement{}, err
	}

	after, err := w.balance.Add(amount)
	if err != nil {
		return Movement{}, err
	}

	return w.apply(Credit, amount, after, now), nil
}

// checkMovement aplica as validações comuns às duas direções.
func (w *Wallet) checkMovement(amount money.Money) error {
	if w == nil || w.currency.IsZero() {
		return ErrUninitialized
	}
	if !amount.IsValid() {
		return money.ErrUninitialized
	}
	if !amount.IsPositive() {
		return fmt.Errorf("%w: %s", ErrNonPositiveAmount, amount)
	}
	// A moeda também é imposta pelo banco, por chave estrangeira composta. A
	// verificação aqui existe para dar mensagem útil antes de a operação chegar
	// lá — não para substituir a garantia.
	if amount.Currency() != w.currency {
		return fmt.Errorf("%w: carteira em %s, movimentação em %s",
			ErrCurrencyMismatch, w.currency, amount.Currency())
	}
	return nil
}

// apply muda o estado e devolve a movimentação correspondente.
func (w *Wallet) apply(dir Direction, amount, after money.Money, now time.Time) Movement {
	before := w.balance
	now = now.UTC()

	w.balance = after
	w.version++
	w.updatedAt = now

	return Movement{
		Direction:     dir,
		Amount:        amount,
		BalanceBefore: before,
		BalanceAfter:  after,
		Version:       w.version,
		OccurredAt:    now,
	}
}

// Acessores. Não há setter: o estado só muda pelas operações acima.

func (w *Wallet) ID() ID                   { return w.id }
func (w *Wallet) PlayerID() PlayerID       { return w.playerID }
func (w *Wallet) Currency() money.Currency { return w.currency }
func (w *Wallet) Balance() money.Money     { return w.balance }
func (w *Wallet) Version() int64           { return w.version }
func (w *Wallet) CreatedAt() time.Time     { return w.createdAt }
func (w *Wallet) UpdatedAt() time.Time     { return w.updatedAt }

// BelongsTo informa se a carteira é do jogador indicado.
func (w *Wallet) BelongsTo(playerID PlayerID) bool { return w.playerID == playerID }
