package money

import (
	"encoding/json"
	"fmt"
)

// moneyJSON é a forma de fio: {"amount":"25.00","currency":"BRL"}.
//
// Os campos são ponteiros para distinguir ausente de vazio. E o valor é string,
// não número: um número JSON vira float64 na maioria dos leitores, e o valor
// perderia precisão antes de o código Go sequer vê-lo. Como o campo é *string,
// um número no corpo falha na desserialização em vez de ser convertido.
type moneyJSON struct {
	Amount   *string `json:"amount"`
	Currency *string `json:"currency"`
}

// MarshalJSON serializa no formato do contrato externo.
func (m Money) MarshalJSON() ([]byte, error) {
	if err := m.validate(); err != nil {
		return nil, err
	}
	amount := m.Amount()
	currency := m.currency.String()
	return json.Marshal(moneyJSON{Amount: &amount, Currency: &currency})
}

// UnmarshalJSON lê e valida o formato do contrato externo.
//
// Toda recusa acontece aqui, na fronteira: um Money só existe depois de o valor
// e a moeda terem sido aceitos, então nenhuma camada adiante precisa reconferir.
func (m *Money) UnmarshalJSON(data []byte) error {
	var raw moneyJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidAmount, err)
	}

	if raw.Amount == nil {
		return fmt.Errorf("%w: campo amount ausente", ErrInvalidAmount)
	}
	if raw.Currency == nil {
		return fmt.Errorf("%w: campo currency ausente", ErrInvalidCurrency)
	}

	currency, err := ParseCurrency(*raw.Currency)
	if err != nil {
		return err
	}

	parsed, err := Parse(*raw.Amount, currency)
	if err != nil {
		return err
	}

	*m = parsed
	return nil
}
