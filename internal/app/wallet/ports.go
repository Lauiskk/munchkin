// Package wallet implementa os casos de uso de carteira.
//
// As interfaces de que o caso de uso precisa são declaradas AQUI, não no
// adaptador. É o caso de uso que diz do que precisa; o adaptador se adapta. A
// direção oposta faria a regra de negócio depender do formato do banco.
package wallet

import (
	"context"

	"github.com/Lauiskk/munchkin/internal/domain/event"
	"github.com/Lauiskk/munchkin/internal/domain/ledger"
	"github.com/Lauiskk/munchkin/internal/domain/wagering"
	domain "github.com/Lauiskk/munchkin/internal/domain/wallet"
)

// TxManager delimita a transação.
type TxManager interface {
	// Within executa fn dentro de uma transação, confirmando ao final e
	// desfazendo em caso de erro ou pânico.
	Within(ctx context.Context, fn func(ctx context.Context) error) error
}

// Repository persiste carteiras.
type Repository interface {
	Create(ctx context.Context, w *domain.Wallet) error
	FindByID(ctx context.Context, id domain.ID) (*domain.Wallet, error)
}

// TransactionRepository persiste transações de aposta.
type TransactionRepository interface {
	Create(ctx context.Context, t *wagering.Transaction) error
}

// LedgerRepository grava lançamentos.
//
// Só grava. A ausência de Update e Delete na interface é a expressão do
// append-only na camada que o caso de uso enxerga.
type LedgerRepository interface {
	Append(ctx context.Context, e ledger.Entry) error
}

// OutboxRepository grava eventos para publicação posterior.
type OutboxRepository interface {
	Enqueue(ctx context.Context, env event.Envelope) error
}
