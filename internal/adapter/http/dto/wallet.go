package dto

import (
	"github.com/Lauiskk/munchkin/internal/domain/money"
	"github.com/Lauiskk/munchkin/internal/domain/wallet"
	"github.com/Lauiskk/munchkin/pkg/apperr"
)

// MoneyPayload é a forma de fio de um valor monetário.
//
// O valor é string, nunca número: número JSON vira ponto flutuante na maioria
// dos leitores, e o valor perderia precisão antes de o código Go vê-lo.
type MoneyPayload struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// OpenWalletRequest é o corpo da abertura de carteira.
type OpenWalletRequest struct {
	PlayerID       string       `json:"playerId"`
	InitialBalance MoneyPayload `json:"initialBalance"`
}

// Decode valida e converte para os tipos de domínio.
//
// Note o que NÃO está aqui: o identificador da carteira. Ele é gerado pelo
// servidor. Aceitá-lo do corpo permitiria colisão deliberada e sondagem de
// identificadores existentes.
func (r OpenWalletRequest) Decode() (wallet.PlayerID, money.Money, error) {
	var campos Fields

	playerID, err := wallet.ParsePlayerID(r.PlayerID)
	if err != nil {
		campos.AddErr("playerId", err, apperr.FieldCodeInvalidFormat)
	}

	var saldo money.Money
	currency, err := money.ParseCurrency(r.InitialBalance.Currency)
	if err != nil {
		campos.AddErr("initialBalance.currency", err, apperr.FieldCodeInvalidValue)
	} else {
		saldo, err = money.Parse(r.InitialBalance.Amount, currency)
		if err != nil {
			campos.AddErr("initialBalance.amount", err, apperr.FieldCodeInvalidFormat)
		} else if saldo.IsNegative() {
			// Negativo é representável no tipo — diferenças precisam dele —,
			// mas não é aceitável numa entrada financeira externa.
			campos.Add("initialBalance.amount",
				"saldo inicial não pode ser negativo", apperr.FieldCodeInvalidValue)
		}
	}

	if err := campos.Err(); err != nil {
		return wallet.PlayerID{}, money.Money{}, err
	}
	return playerID, saldo, nil
}

// WalletResponse é a representação da carteira na resposta.
type WalletResponse struct {
	ID       wallet.ID       `json:"id"`
	PlayerID wallet.PlayerID `json:"playerId"`
	Balance  money.Money     `json:"balance"`
	Version  int64           `json:"version"`
}

// NewWalletResponse monta a resposta a partir do agregado.
func NewWalletResponse(w *wallet.Wallet) WalletResponse {
	return WalletResponse{
		ID:       w.ID(),
		PlayerID: w.PlayerID(),
		Balance:  w.Balance(),
		Version:  w.Version(),
	}
}
