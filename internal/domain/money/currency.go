package money

import (
	"errors"
	"fmt"
)

// ErrInvalidCurrency indica código de moeda ausente ou fora do formato ISO 4217.
var ErrInvalidCurrency = errors.New("moeda inválida")

// Currency é um código ISO 4217 validado.
//
// O campo é não exportado de propósito: Currency("xx") não compila fora deste
// pacote, então toda moeda em circulação passou por ParseCurrency ou é uma das
// constantes abaixo. É o que impede uma string qualquer de se apresentar como
// moeda no meio de um cálculo financeiro.
type Currency struct {
	code string
}

// Moedas conhecidas. A lista é fechada porque aceitar qualquer sequência de
// três letras maiúsculas deixaria passar "XYZ" — sintaticamente válido e sem
// significado. Novas moedas entram aqui, conscientemente.
var (
	BRL = Currency{code: "BRL"}
	USD = Currency{code: "USD"}
	EUR = Currency{code: "EUR"}
)

var conhecidas = map[string]Currency{
	BRL.code: BRL,
	USD.code: USD,
	EUR.code: EUR,
}

// ParseCurrency valida e converte um código de moeda.
//
// A comparação é exata, sem normalizar caixa: "brl" é recusado em vez de
// convertido. Aceitar variações obrigaria a documentar a normalização antes do
// hash de idempotência, e duas grafias da mesma moeda produziriam hashes
// diferentes para o mesmo negócio.
func ParseCurrency(code string) (Currency, error) {
	c, ok := conhecidas[code]
	if !ok {
		return Currency{}, fmt.Errorf("%w: %q não é um código ISO 4217 reconhecido",
			ErrInvalidCurrency, truncar(code))
	}
	return c, nil
}

// String devolve o código ISO 4217.
func (c Currency) String() string { return c.code }

// IsZero informa se a moeda não foi inicializada.
func (c Currency) IsZero() bool { return c.code == "" }

// MarshalJSON serializa como o código.
func (c Currency) MarshalJSON() ([]byte, error) {
	if c.IsZero() {
		return nil, ErrInvalidCurrency
	}
	return []byte(`"` + c.code + `"`), nil
}

// UnmarshalJSON valida ao ler.
func (c *Currency) UnmarshalJSON(data []byte) error {
	if len(data) < 2 || data[0] != '"' || data[len(data)-1] != '"' {
		return fmt.Errorf("%w: esperava string com o código", ErrInvalidCurrency)
	}
	parsed, err := ParseCurrency(string(data[1 : len(data)-1]))
	if err != nil {
		return err
	}
	*c = parsed
	return nil
}
