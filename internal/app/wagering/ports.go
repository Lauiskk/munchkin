// Package wagering implementa os casos de uso de operação financeira.
package wagering

import (
	"context"

	"github.com/Lauiskk/munchkin/internal/domain/event"
	"github.com/Lauiskk/munchkin/internal/domain/ledger"
	domain "github.com/Lauiskk/munchkin/internal/domain/wagering"
	"github.com/Lauiskk/munchkin/internal/domain/wallet"
)

// TxManager delimita a transação.
type TxManager interface {
	Within(ctx context.Context, fn func(ctx context.Context) error) error
}

// WalletRepository lê e escreve carteiras.
type WalletRepository interface {
	// LockByID lê a carteira travando a linha até o fim da transação. É o
	// ponto de coordenação: serializa os escritores daquela carteira, e só
	// dela.
	LockByID(ctx context.Context, id wallet.ID) (*wallet.Wallet, error)
	// UpdateBalance grava o saldo exigindo que a versão esperada ainda valha.
	UpdateBalance(ctx context.Context, w *wallet.Wallet, versaoAnterior int64) error
}

// ClaimResult descreve o desfecho de uma reivindicação de idempotência.
type ClaimResult struct {
	Claimed  bool
	Existing *domain.Transaction
}

// TransactionRepository persiste operações.
type TransactionRepository interface {
	// Claim insere a operação. É a própria verificação de idempotência: o
	// índice único decide, sem consulta prévia.
	Claim(ctx context.Context, t *domain.Transaction) (ClaimResult, error)
	// Settle grava o desfecho, exigindo que o estado ainda seja não terminal.
	Settle(ctx context.Context, t *domain.Transaction) error
	// FindByID busca pela identidade interna.
	FindByID(ctx context.Context, id domain.TransactionID) (*domain.Transaction, error)
	// FindByProviderExternalID busca pela identidade da operação no provedor.
	FindByProviderExternalID(ctx context.Context, p domain.ProviderID, e domain.ExternalID) (*domain.Transaction, error)
	// FindProcessedReversalOf busca a reversão já concluída de uma referência.
	//
	// Existe para que "esta referência já foi revertida" seja respondido como
	// recusa de negócio, com código próprio, e não como o erro do índice único
	// que existe para impedir a segunda gravação. O índice continua sendo a
	// autoridade; esta consulta é o que produz a resposta legível.
	FindProcessedReversalOf(ctx context.Context, ref domain.TransactionID) (*domain.Transaction, error)
}

// LedgerRepository grava lançamentos. Só grava: o ledger é append-only.
type LedgerRepository interface {
	Append(ctx context.Context, e ledger.Entry) error
}

// OutboxRepository grava eventos para publicação posterior.
type OutboxRepository interface {
	Enqueue(ctx context.Context, env event.Envelope) error
}
