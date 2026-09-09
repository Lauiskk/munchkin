package dto

import (
	"github.com/Lauiskk/munchkin/internal/domain/money"
	"github.com/Lauiskk/munchkin/internal/domain/wagering"
	"github.com/Lauiskk/munchkin/internal/domain/wallet"
)

// TransactionResponse é a resposta de uma operação.
type TransactionResponse struct {
	TransactionID    *wagering.TransactionID `json:"transactionId,omitempty"`
	Status           wagering.Status         `json:"status"`
	Balance          *money.Money            `json:"balance,omitempty"`
	FailureCode      wagering.FailureCode    `json:"failureCode,omitempty"`
	IdempotentReplay bool                    `json:"idempotentReplay"`
}

// TransactionDetail é a representação de uma operação na consulta.
type TransactionDetail struct {
	TransactionID         wagering.TransactionID `json:"transactionId"`
	ExternalTransactionID wagering.ExternalID    `json:"externalTransactionId"`
	ProviderID            wagering.ProviderID    `json:"providerId"`
	WalletID              wallet.ID              `json:"walletId"`
	PlayerID              wallet.PlayerID        `json:"playerId"`
	Kind                  wagering.Kind          `json:"kind"`
	Status                wagering.Status        `json:"status"`
	Money                 money.Money            `json:"money"`
	Balance               *money.Money           `json:"balance,omitempty"`
	FailureCode           wagering.FailureCode   `json:"failureCode,omitempty"`
	Attempts              int                    `json:"attempts,omitempty"`
}

// NewTransactionDetail monta a representação a partir do agregado.
func NewTransactionDetail(t *wagering.Transaction) TransactionDetail {
	d := TransactionDetail{
		TransactionID: t.ID(),
		WalletID:      t.WalletID(),
		PlayerID:      t.PlayerID(),
		Kind:          t.Kind(),
		Status:        t.Status(),
		Money:         t.Amount(),
		FailureCode:   t.FailureCode(),
		Attempts:      t.Attempts(),
	}
	if o := t.Origin(); o != nil {
		d.ProviderID = o.ProviderID
		d.ExternalTransactionID = o.ExternalID
	}
	if saldo, ok := t.ResultBalance(); ok {
		d.Balance = &saldo
	}
	return d
}
