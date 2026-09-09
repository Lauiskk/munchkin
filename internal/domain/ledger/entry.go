// Package ledger implementa o lançamento contábil da carteira.
//
// O lançamento é imutável: uma vez construído, não há método que o altere. A
// imutabilidade também é imposta pelo banco, por gatilho e por privilégio
// revogado — correção financeira é lançamento novo, nunca edição.
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

var (
	// ErrInvalidID indica identificador ausente ou malformado.
	ErrInvalidID = errors.New("identificador de lançamento inválido")
	// ErrBalanceMismatch indica lançamento cuja aritmética não fecha.
	ErrBalanceMismatch = errors.New("o lançamento não fecha a conta")
	// ErrNonPositiveAmount indica lançamento de valor zero ou negativo.
	ErrNonPositiveAmount = errors.New("lançamento exige valor positivo")
	// ErrNegativeBalance indica saldo negativo em um dos extremos.
	ErrNegativeBalance = errors.New("saldo do lançamento não pode ser negativo")
	// ErrCurrencyMismatch indica moedas divergentes dentro do lançamento.
	ErrCurrencyMismatch = errors.New("moedas divergentes no lançamento")
)

// ID identifica um lançamento.
type ID uuid.UUID

// NewID gera um identificador de lançamento.
func NewID() (ID, error) {
	u, err := uuid.NewV7()
	if err != nil {
		return ID{}, fmt.Errorf("geração de identificador de lançamento: %w", err)
	}
	return ID(u), nil
}

// ParseID converte a representação textual.
func ParseID(s string) (ID, error) {
	u, err := uuid.Parse(s)
	if err != nil || u == uuid.Nil {
		return ID{}, fmt.Errorf("%w: %q", ErrInvalidID, s)
	}
	return ID(u), nil
}

func (i ID) String() string { return uuid.UUID(i).String() }
func (i ID) IsZero() bool   { return i == ID{} }

// Entry é um lançamento do ledger.
type Entry struct {
	id            ID
	walletID      wallet.ID
	transactionID wagering.TransactionID
	direction     wallet.Direction
	amount        money.Money
	balanceBefore money.Money
	balanceAfter  money.Money
	createdAt     time.Time
}

// New constrói um lançamento, conferindo a aritmética.
//
// A conferência é a invariante central do ledger: um lançamento que não fecha a
// conta não entra. É por causa dela que a reconciliação pode reconstruir o saldo
// somando os lançamentos e confiar no resultado. O banco confere a mesma coisa
// por CHECK — as duas camadas existem porque a do domínio dá mensagem útil e a
// do banco é a que garante.
func New(
	id ID, walletID wallet.ID, transactionID wagering.TransactionID,
	direction wallet.Direction, amount, balanceBefore, balanceAfter money.Money,
	now time.Time,
) (Entry, error) {
	if id.IsZero() {
		return Entry{}, fmt.Errorf("%w: ausente", ErrInvalidID)
	}
	if walletID.IsZero() || transactionID.IsZero() {
		return Entry{}, fmt.Errorf("%w: carteira ou transação ausente", ErrInvalidID)
	}
	if direction != wallet.Debit && direction != wallet.Credit {
		return Entry{}, fmt.Errorf("direção inválida: %q", direction)
	}
	for _, m := range []money.Money{amount, balanceBefore, balanceAfter} {
		if !m.IsValid() {
			return Entry{}, money.ErrUninitialized
		}
	}
	if amount.Currency() != balanceBefore.Currency() ||
		amount.Currency() != balanceAfter.Currency() {
		return Entry{}, fmt.Errorf("%w: %s, %s e %s", ErrCurrencyMismatch,
			amount.Currency(), balanceBefore.Currency(), balanceAfter.Currency())
	}
	if !amount.IsPositive() {
		return Entry{}, fmt.Errorf("%w: %s", ErrNonPositiveAmount, amount)
	}
	if balanceBefore.IsNegative() || balanceAfter.IsNegative() {
		return Entry{}, fmt.Errorf("%w: %s para %s", ErrNegativeBalance, balanceBefore, balanceAfter)
	}

	esperado, err := aritmetica(direction, balanceBefore, amount)
	if err != nil {
		return Entry{}, err
	}
	if !esperado.Equal(balanceAfter) {
		return Entry{}, fmt.Errorf("%w: %s %s %s deveria dar %s, informado %s",
			ErrBalanceMismatch, balanceBefore, sinal(direction), amount, esperado, balanceAfter)
	}

	return Entry{
		id: id, walletID: walletID, transactionID: transactionID,
		direction: direction, amount: amount,
		balanceBefore: balanceBefore, balanceAfter: balanceAfter,
		createdAt: now.UTC(),
	}, nil
}

// FromMovement constrói o lançamento a partir da movimentação que o agregado
// produziu.
//
// É o caminho normal: usar a movimentação em vez de recalcular os saldos fora do
// agregado elimina a chance de o ledger discordar da carteira.
func FromMovement(
	id ID, walletID wallet.ID, transactionID wagering.TransactionID,
	mv wallet.Movement, now time.Time,
) (Entry, error) {
	return New(id, walletID, transactionID, mv.Direction,
		mv.Amount, mv.BalanceBefore, mv.BalanceAfter, now)
}

func aritmetica(direction wallet.Direction, before, amount money.Money) (money.Money, error) {
	if direction == wallet.Credit {
		return before.Add(amount)
	}
	return before.Sub(amount)
}

func sinal(direction wallet.Direction) string {
	if direction == wallet.Credit {
		return "+"
	}
	return "-"
}

// Acessores. Não há setter: o lançamento é imutável por construção.

func (e Entry) ID() ID                                { return e.id }
func (e Entry) WalletID() wallet.ID                   { return e.walletID }
func (e Entry) TransactionID() wagering.TransactionID { return e.transactionID }
func (e Entry) Direction() wallet.Direction           { return e.direction }
func (e Entry) Amount() money.Money                   { return e.amount }
func (e Entry) BalanceBefore() money.Money            { return e.balanceBefore }
func (e Entry) BalanceAfter() money.Money             { return e.balanceAfter }
func (e Entry) CreatedAt() time.Time                  { return e.createdAt }

// SignedAmount devolve o valor com sinal, para reconstruir saldo por soma.
func (e Entry) SignedAmount() (money.Money, error) {
	if e.direction == wallet.Credit {
		return e.amount, nil
	}
	return e.amount.Neg()
}

// MarshalText serializa como texto.
//
// Necessário: ID é definido sobre uuid.UUID, que é [16]byte. Tipos
// definidos não herdam métodos, então sem isto o identificador sairia como
// array de números no JSON — ilegível para o consumidor e inútil para
// correlacionar.
func (i ID) MarshalText() ([]byte, error) { return []byte(i.String()), nil }

// UnmarshalText lê a representação textual, validando.
func (i *ID) UnmarshalText(data []byte) error {
	parsed, err := ParseID(string(data))
	if err != nil {
		return err
	}
	*i = parsed
	return nil
}
