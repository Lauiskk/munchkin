package dto

import (
	"github.com/Lauiskk/munchkin/internal/domain/money"
	"github.com/Lauiskk/munchkin/internal/domain/wagering"
	"github.com/Lauiskk/munchkin/internal/domain/wallet"
	"github.com/Lauiskk/munchkin/pkg/apperr"
)

// SubmitTransactionRequest é o corpo de uma operação financeira.
type SubmitTransactionRequest struct {
	ProviderID            string       `json:"providerId"`
	ExternalTransactionID string       `json:"externalTransactionId"`
	PlayerID              string       `json:"playerId"`
	WalletID              string       `json:"walletId"`
	RoundID               string       `json:"roundId"`
	GameID                string       `json:"gameId"`
	Kind                  string       `json:"kind"`
	Money                 MoneyPayload `json:"money"`

	ReferenceExternalTransactionID string `json:"referenceExternalTransactionId,omitempty"`
}

// DecodedTransaction são os campos já convertidos para tipos de domínio.
type DecodedTransaction struct {
	ProviderID          wagering.ProviderID
	ExternalID          wagering.ExternalID
	PlayerID            wallet.PlayerID
	WalletID            wallet.ID
	RoundID             wagering.RoundID
	GameID              wagering.GameID
	Kind                wagering.Kind
	Money               money.Money
	ReferenceExternalID wagering.ExternalID
}

// Decode valida e converte, acumulando os erros por campo.
func (r SubmitTransactionRequest) Decode() (DecodedTransaction, error) {
	var campos Fields
	var out DecodedTransaction

	if v, err := wagering.ParseProviderID(r.ProviderID); err != nil {
		campos.AddErr("providerId", err, apperr.FieldCodeInvalidFormat)
	} else {
		out.ProviderID = v
	}
	if v, err := wagering.ParseExternalID(r.ExternalTransactionID); err != nil {
		campos.AddErr("externalTransactionId", err, apperr.FieldCodeInvalidFormat)
	} else {
		out.ExternalID = v
	}
	if v, err := wallet.ParsePlayerID(r.PlayerID); err != nil {
		campos.AddErr("playerId", err, apperr.FieldCodeInvalidFormat)
	} else {
		out.PlayerID = v
	}
	if v, err := wallet.ParseID(r.WalletID); err != nil {
		campos.AddErr("walletId", err, apperr.FieldCodeInvalidFormat)
	} else {
		out.WalletID = v
	}
	if v, err := wagering.ParseRoundID(r.RoundID); err != nil {
		campos.AddErr("roundId", err, apperr.FieldCodeInvalidFormat)
	} else {
		out.RoundID = v
	}
	if v, err := wagering.ParseGameID(r.GameID); err != nil {
		campos.AddErr("gameId", err, apperr.FieldCodeInvalidFormat)
	} else {
		out.GameID = v
	}

	// ParseExternalKind recusa OPENING: é a abertura interna de carteira, e um
	// provedor que pudesse enviá-la creditaria a própria carteira.
	if v, err := wagering.ParseExternalKind(r.Kind); err != nil {
		campos.AddErr("kind", err, apperr.FieldCodeInvalidValue)
	} else {
		out.Kind = v
	}

	if currency, err := money.ParseCurrency(r.Money.Currency); err != nil {
		campos.AddErr("money.currency", err, apperr.FieldCodeInvalidValue)
	} else if valor, err := money.Parse(r.Money.Amount, currency); err != nil {
		campos.AddErr("money.amount", err, apperr.FieldCodeInvalidFormat)
	} else if valor.IsNegative() {
		campos.Add("money.amount", "valor negativo não é aceito na entrada",
			apperr.FieldCodeInvalidValue)
	} else {
		out.Money = valor
	}

	if r.ReferenceExternalTransactionID != "" {
		if v, err := wagering.ParseExternalID(r.ReferenceExternalTransactionID); err != nil {
			campos.AddErr("referenceExternalTransactionId", err, apperr.FieldCodeInvalidFormat)
		} else {
			out.ReferenceExternalID = v
		}
	}

	if err := campos.Err(); err != nil {
		return DecodedTransaction{}, err
	}
	return out, nil
}

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
