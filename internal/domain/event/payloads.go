package event

import (
	"github.com/Lauiskk/munchkin/internal/domain/money"
	"github.com/Lauiskk/munchkin/internal/domain/wagering"
	"github.com/Lauiskk/munchkin/internal/domain/wallet"
)

// Nomes dos eventos. São contrato: mudar um quebra integração alheia.
const (
	TypeWagerTransactionProcessed        Type = "WagerTransactionProcessed"
	TypeWagerTransactionRejected         Type = "WagerTransactionRejected"
	TypeWagerTransactionPendingReference Type = "WagerTransactionPendingReference"
	TypeWalletBalanceChanged             Type = "WalletBalanceChanged"
)

// WagerTransactionProcessed anuncia a conclusão bem-sucedida de uma operação,
// inclusive LOSS — que conclui sem movimentar a carteira.
type WagerTransactionProcessed struct {
	TransactionID wagering.TransactionID `json:"transactionId"`
	WalletID      wallet.ID              `json:"walletId"`
	PlayerID      wallet.PlayerID        `json:"playerId"`
	Kind          wagering.Kind          `json:"kind"`
	Money         money.Money            `json:"money"`
	Balance       money.Money            `json:"balance"`
	// Vazios para a abertura interna de carteira, que não tem provedor.
	ProviderID  wagering.ProviderID `json:"providerId,omitempty"`
	ExternalID  wagering.ExternalID `json:"externalTransactionId,omitempty"`
	RoundID     wagering.RoundID    `json:"roundId,omitempty"`
	GameID      wagering.GameID     `json:"gameId,omitempty"`
	ProcessedAt Timestamp           `json:"processedAt"`
}

func (WagerTransactionProcessed) EventType() Type          { return TypeWagerTransactionProcessed }
func (WagerTransactionProcessed) EventVersion() int        { return 1 }
func (e WagerTransactionProcessed) AggregateID() wallet.ID { return e.WalletID }

// WagerTransactionRejected anuncia recusa definitiva por regra de negócio.
type WagerTransactionRejected struct {
	TransactionID wagering.TransactionID `json:"transactionId"`
	WalletID      wallet.ID              `json:"walletId"`
	PlayerID      wallet.PlayerID        `json:"playerId"`
	Kind          wagering.Kind          `json:"kind"`
	Money         money.Money            `json:"money"`
	// FailureCode é o motivo estável da recusa, e é por ele que o provedor
	// decide o que fazer.
	FailureCode wagering.FailureCode `json:"failureCode"`
	ProviderID  wagering.ProviderID  `json:"providerId,omitempty"`
	ExternalID  wagering.ExternalID  `json:"externalTransactionId,omitempty"`
	RejectedAt  Timestamp            `json:"rejectedAt"`
}

func (WagerTransactionRejected) EventType() Type          { return TypeWagerTransactionRejected }
func (WagerTransactionRejected) EventVersion() int        { return 1 }
func (e WagerTransactionRejected) AggregateID() wallet.ID { return e.WalletID }

// WagerTransactionPendingReference anuncia que a operação aguarda uma referência
// ainda não recebida.
type WagerTransactionPendingReference struct {
	TransactionID       wagering.TransactionID `json:"transactionId"`
	WalletID            wallet.ID              `json:"walletId"`
	PlayerID            wallet.PlayerID        `json:"playerId"`
	Kind                wagering.Kind          `json:"kind"`
	Money               money.Money            `json:"money"`
	ProviderID          wagering.ProviderID    `json:"providerId"`
	ExternalID          wagering.ExternalID    `json:"externalTransactionId"`
	ReferenceExternalID wagering.ExternalID    `json:"referenceExternalTransactionId"`
	PendingSince        Timestamp              `json:"pendingSince"`
}

func (WagerTransactionPendingReference) EventType() Type          { return TypeWagerTransactionPendingReference }
func (WagerTransactionPendingReference) EventVersion() int        { return 1 }
func (e WagerTransactionPendingReference) AggregateID() wallet.ID { return e.WalletID }

// WalletBalanceChanged anuncia alteração efetiva do saldo.
//
// O conteúdo é o exigido pelo §11: carteira, transação, direção, valor, saldo
// anterior, saldo posterior e versão da carteira. Com esses campos, um consumidor
// consegue reconstruir o saldo sem consultar a API.
type WalletBalanceChanged struct {
	WalletID      wallet.ID              `json:"walletId"`
	TransactionID wagering.TransactionID `json:"transactionId"`
	Direction     wallet.Direction       `json:"direction"`
	Money         money.Money            `json:"money"`
	BalanceBefore money.Money            `json:"balanceBefore"`
	BalanceAfter  money.Money            `json:"balanceAfter"`
	WalletVersion int64                  `json:"walletVersion"`
	ChangedAt     Timestamp              `json:"changedAt"`
}

func (WalletBalanceChanged) EventType() Type          { return TypeWalletBalanceChanged }
func (WalletBalanceChanged) EventVersion() int        { return 1 }
func (e WalletBalanceChanged) AggregateID() wallet.ID { return e.WalletID }
