// Package contract carrega a forma de fio das operações financeiras, comum a
// todos os transportes.
//
// Não mora sob `http/` porque não é de HTTP: a mesma operação entra por
// requisição e por mensagem, e o §8 do enunciado exige que as duas produzam o
// mesmo hash de idempotência. Duas cópias da mesma validação divergem, e a
// divergência apareceria justamente ali — como duas operações distintas para o
// que é a mesma.
package contract

import (
	"github.com/Lauiskk/munchkin/internal/domain/money"
	"github.com/Lauiskk/munchkin/internal/domain/wagering"
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
		// Referência num tipo que não a admite é campo mal preenchido, e a
		// fronteira é onde isso se recusa: 400 apontando o campo diz o que
		// corrigir, enquanto deixar seguir devolveria um erro de domínio genérico
		// mais adiante. O domínio mantém a mesma regra como defesa em
		// profundidade — quem entra pela fila não passa por aqui.
		if out.Kind != "" && !out.Kind.AcceptsReference() {
			campos.Add("referenceExternalTransactionId",
				"só REFUND, ROLLBACK e WIN admitem referência",
				apperr.FieldCodeInvalidValue)
		}
	}

	if err := campos.Err(); err != nil {
		return DecodedTransaction{}, err
	}
	return out, nil
}
