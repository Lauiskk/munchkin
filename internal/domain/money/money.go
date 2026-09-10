// Package money implementa o valor monetário do domínio.
//
// A representação é `int64` em unidades mínimas — centavos, para as moedas de
// duas casas. Não há ponto flutuante em lugar nenhum, nem como etapa
// intermediária do parsing: base 2 não representa décimos exatamente, e num
// sistema financeiro isso vira centavo que some. Há um gate de CI que recusa
// `float32`, `float64`, `ParseFloat` e `FormatFloat` neste caminho.
//
// Faixa representável: de -92.233.720.368.547.758,07 a +92.233.720.368.547.758,07.
// A faixa é simétrica porque o menor valor de `int64` é recusado na construção:
// sua negação não seria representável, e um valor que quebra uma operação
// futura não deve existir.
package money

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"unicode/utf8"
)

// Scale é a quantidade de casas decimais. Fixa em duas, conforme o contrato.
const Scale = 2

// unitsPerMajor é quantas unidades mínimas formam uma unidade da moeda.
const unitsPerMajor int64 = 100

// maxInputLen limita a entrada aceita pelo parser.
//
// O maior valor representável tem 17 dígitos inteiros, mais o ponto, mais duas
// casas, mais o sinal: 21 caracteres. Recusar acima disso pelo tamanho, antes de
// olhar o conteúdo, impede que uma entrada enorme vinda de um provedor consuma
// tempo e apareça no log.
const maxInputLen = 21

var (
	// ErrUninitialized indica valor monetário que nunca foi construído.
	ErrUninitialized = errors.New("valor monetário não inicializado")
	// ErrInvalidAmount indica string que não é um decimal de escala fixa.
	ErrInvalidAmount = errors.New("valor monetário inválido")
	// ErrCurrencyMismatch indica operação entre moedas diferentes.
	ErrCurrencyMismatch = errors.New("moedas incompatíveis")
	// ErrOverflow indica resultado fora da faixa representável.
	ErrOverflow = errors.New("valor monetário fora da faixa representável")
)

// Money é um valor monetário imutável.
//
// Os campos são não exportados: um Money só existe através dos construtores, e
// nenhum código de fora consegue montar um com moeda vazia ou valor
// inconsistente. Todas as operações devolvem um valor novo.
type Money struct {
	minor    int64
	currency Currency
}

// New constrói a partir de unidades mínimas. É o caminho da reidratação, quando
// o valor vem do banco já na representação interna.
func New(minor int64, currency Currency) (Money, error) {
	if currency.IsZero() {
		return Money{}, fmt.Errorf("%w: moeda ausente", ErrInvalidCurrency)
	}
	// O menor int64 é recusado porque sua negação não é representável. Aceitá-lo
	// criaria um valor legítimo que faz Neg estourar depois, longe daqui.
	if minor == math.MinInt64 {
		return Money{}, fmt.Errorf("%w: %d não tem negação representável", ErrOverflow, minor)
	}
	return Money{minor: minor, currency: currency}, nil
}

// Zero devolve o valor nulo da moeda.
func Zero(currency Currency) (Money, error) { return New(0, currency) }

// ZeroOfSame devolve o valor nulo na mesma moeda, sem poder falhar quando o
// receptor é válido. Evita propagar erro em comparações internas.
func (m Money) ZeroOfSame() Money { return Money{minor: 0, currency: m.currency} }

// Parse constrói a partir de um decimal de escala fixa em duas casas.
//
// O parsing é feito dígito a dígito. Não passa por strconv.ParseFloat em momento
// nenhum, nem como etapa intermediária — é essa a diferença entre "não usamos
// float" e "não há float".
//
// O formato aceito é exato: sinal opcional, parte inteira sem zeros à esquerda,
// ponto, exatamente duas casas. Recusar variações equivalentes ("025.00",
// "25.0", " 25.00") é deliberado: aceitá-las obrigaria a documentar uma
// normalização anterior ao hash de idempotência, e duas grafias do mesmo valor
// produziriam hashes diferentes para o mesmo negócio.
func Parse(amount string, currency Currency) (Money, error) {
	if currency.IsZero() {
		return Money{}, fmt.Errorf("%w: moeda ausente", ErrInvalidCurrency)
	}

	minor, err := parseMinor(amount)
	if err != nil {
		return Money{}, err
	}
	return New(minor, currency)
}

// parseMinor converte o decimal em unidades mínimas.
func parseMinor(s string) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("%w: vazio", ErrInvalidAmount)
	}
	if len(s) > maxInputLen {
		return 0, fmt.Errorf("%w: %d caracteres excedem o máximo de %d",
			ErrInvalidAmount, len(s), maxInputLen)
	}

	negative := false
	if s[0] == '-' {
		negative = true
		s = s[1:]
		if s == "" {
			return 0, fmt.Errorf("%w: sinal sem valor", ErrInvalidAmount)
		}
	}

	dot := -1
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			if dot >= 0 {
				return 0, fmt.Errorf("%w: mais de um separador decimal", ErrInvalidAmount)
			}
			dot = i
		}
	}
	if dot < 0 {
		return 0, fmt.Errorf("%w: são exigidas exatamente %d casas decimais",
			ErrInvalidAmount, Scale)
	}

	integer, fraction := s[:dot], s[dot+1:]

	if len(fraction) != Scale {
		return 0, fmt.Errorf("%w: %d casas decimais, esperava exatamente %d",
			ErrInvalidAmount, len(fraction), Scale)
	}
	if integer == "" {
		return 0, fmt.Errorf("%w: parte inteira ausente", ErrInvalidAmount)
	}
	// "025.00" e "25.00" seriam o mesmo valor com grafias diferentes. Recusar a
	// forma com zeros à esquerda mantém uma grafia por valor.
	if len(integer) > 1 && integer[0] == '0' {
		return 0, fmt.Errorf("%w: zeros à esquerda não são aceitos", ErrInvalidAmount)
	}

	units, err := parseDigits(integer)
	if err != nil {
		return 0, err
	}
	cents, err := parseDigits(fraction)
	if err != nil {
		return 0, err
	}

	// units*100 + cents, com o estouro conferido ANTES de cada operação.
	if units > (math.MaxInt64-cents)/unitsPerMajor {
		return 0, fmt.Errorf("%w: valor excede o representável", ErrOverflow)
	}
	total := units*unitsPerMajor + cents

	if negative {
		// Zero negativo é a mesma quantia que zero, escrita de outro jeito.
		// Aceitá-lo criaria duas grafias para um valor — e duas grafias
		// produzem hashes de idempotência diferentes para o mesmo negócio.
		// Encontrado por fuzzing, não por inspeção.
		if total == 0 {
			return 0, fmt.Errorf("%w: zero negativo não é uma grafia válida", ErrInvalidAmount)
		}
		total = -total
	}
	return total, nil
}

// parseDigits acumula dígitos ASCII, conferindo estouro a cada passo.
//
// A verificação de dígito é por faixa de byte, e não por unicode.IsDigit: este
// último aceita algarismos de largura completa e de outros sistemas de escrita,
// que não são o que um contrato financeiro em ASCII deveria receber.
func parseDigits(s string) (int64, error) {
	var acc int64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%w: %q não é um algarismo", ErrInvalidAmount, string(c))
		}
		d := int64(c - '0')
		if acc > (math.MaxInt64-d)/10 {
			return 0, fmt.Errorf("%w: valor excede o representável", ErrOverflow)
		}
		acc = acc*10 + d
	}
	return acc, nil
}

// Minor devolve o valor em unidades mínimas, para a persistência.
func (m Money) Minor() int64 { return m.minor }

// Currency devolve a moeda.
func (m Money) Currency() Currency { return m.currency }

// IsValid informa se o valor foi construído.
func (m Money) IsValid() bool { return !m.currency.IsZero() }

func (m Money) validate() error {
	if !m.IsValid() {
		return ErrUninitialized
	}
	return nil
}

// compatible confere que ambos existem e falam a mesma moeda.
func (m Money) compatible(o Money) error {
	if err := m.validate(); err != nil {
		return err
	}
	if err := o.validate(); err != nil {
		return err
	}
	if m.currency != o.currency {
		return fmt.Errorf("%w: %s e %s", ErrCurrencyMismatch, m.currency, o.currency)
	}
	return nil
}

// Add soma dois valores da mesma moeda.
func (m Money) Add(o Money) (Money, error) {
	if err := m.compatible(o); err != nil {
		return Money{}, err
	}
	// O estouro é conferido antes da soma, comparando contra o limite. Somar e
	// depois olhar o sinal do resultado é o erro clássico: em Go o estouro de
	// inteiro com sinal não gera pânico, ele silenciosamente dá a volta.
	if o.minor > 0 && m.minor > math.MaxInt64-o.minor {
		return Money{}, fmt.Errorf("%w: %s + %s", ErrOverflow, m, o)
	}
	if o.minor < 0 && m.minor < math.MinInt64-o.minor {
		return Money{}, fmt.Errorf("%w: %s + %s", ErrOverflow, m, o)
	}
	return New(m.minor+o.minor, m.currency)
}

// Sub subtrai dois valores da mesma moeda.
//
// Resultado negativo é permitido: diferenças e cálculos internos precisam dele.
// Quem proíbe saldo negativo é a carteira, não o tipo.
func (m Money) Sub(o Money) (Money, error) {
	if err := m.compatible(o); err != nil {
		return Money{}, err
	}
	if o.minor < 0 && m.minor > math.MaxInt64+o.minor {
		return Money{}, fmt.Errorf("%w: %s - %s", ErrOverflow, m, o)
	}
	if o.minor > 0 && m.minor < math.MinInt64+o.minor {
		return Money{}, fmt.Errorf("%w: %s - %s", ErrOverflow, m, o)
	}
	return New(m.minor-o.minor, m.currency)
}

// Neg devolve o valor com o sinal invertido.
//
// Não pode estourar porque o menor int64 é recusado na construção — a faixa
// representável é simétrica por decisão, não por acaso.
func (m Money) Neg() (Money, error) {
	if err := m.validate(); err != nil {
		return Money{}, err
	}
	return New(-m.minor, m.currency)
}

// Cmp compara dois valores da mesma moeda: -1, 0 ou 1.
func (m Money) Cmp(o Money) (int, error) {
	if err := m.compatible(o); err != nil {
		return 0, err
	}
	switch {
	case m.minor < o.minor:
		return -1, nil
	case m.minor > o.minor:
		return 1, nil
	default:
		return 0, nil
	}
}

// Equal informa se os valores são idênticos em valor e moeda. Valores de moedas
// diferentes nunca são iguais — e isso não é erro, é resposta.
func (m Money) Equal(o Money) bool {
	return m.IsValid() && o.IsValid() && m.minor == o.minor && m.currency == o.currency
}

// IsZero informa se o valor é nulo.
func (m Money) IsZero() bool { return m.IsValid() && m.minor == 0 }

// IsPositive informa se o valor é maior que zero.
func (m Money) IsPositive() bool { return m.IsValid() && m.minor > 0 }

// IsNegative informa se o valor é menor que zero.
func (m Money) IsNegative() bool { return m.IsValid() && m.minor < 0 }

// Amount devolve o decimal de escala fixa, sem a moeda: "25.00".
func (m Money) Amount() string {
	if !m.IsValid() {
		return ""
	}

	value, sign := m.minor, ""
	if value < 0 {
		sign = "-"
		value = -value
	}

	units := value / unitsPerMajor
	cents := value % unitsPerMajor

	// A formatação é inteira. Duas casas sempre, inclusive para valor redondo e
	// para zero: "25.00", "0.00".
	return sign + strconv.FormatInt(units, 10) + "." +
		string(rune('0'+cents/10)) + string(rune('0'+cents%10))
}

// String devolve valor e moeda, para log e mensagem de erro.
func (m Money) String() string {
	if !m.IsValid() {
		return "<money inválido>"
	}
	return m.Amount() + " " + m.currency.String()
}

// truncar limita o texto ecoado em mensagem de erro. Entrada de origem externa
// não pode determinar o tamanho do que vai para o log.
func truncar(s string) string {
	const limite = 32
	if len(s) <= limite {
		return s
	}
	// O corte respeita a fronteira do caractere. Cortar por byte parte um
	// caractere multibyte ao meio e produz UTF-8 inválido — que o PostgreSQL
	// recusa numa coluna TEXT e o SQS recusa num atributo de mensagem. A
	// entrada aqui vem de fora, então acento no lugar errado é questão de
	// tempo, não de hipótese.
	corte := limite
	for corte > 0 && !utf8.RuneStart(s[corte]) {
		corte--
	}
	return s[:corte] + "…"
}
