package money

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

// O acúmulo de dígitos tem verificação de estouro própria, mas ela é
// inalcançável através de Parse: maxInputLen limita a parte inteira a 18
// dígitos, e 18 noves cabem em int64. Testá-la direto é o que a mantém viva —
// código defensivo que nenhum teste alcança é código que ninguém percebe
// quebrar.
func TestAcumuloDeDigitosDetectaEstouro(t *testing.T) {
	acima := strings.Repeat("9", 19) // 19 noves excedem MaxInt64

	if _, err := parseDigits(acima); err == nil {
		t.Fatalf("parseDigits(%q) deveria estourar", acima)
	}

	limite := strconv.FormatInt(math.MaxInt64, 10)
	got, err := parseDigits(limite)
	if err != nil {
		t.Fatalf("parseDigits(%q) devolveu erro: %v", limite, err)
	}
	if got != math.MaxInt64 {
		t.Fatalf("parseDigits(%q) = %d, esperava %d", limite, got, math.MaxInt64)
	}
}

// A relação entre maxInputLen e a faixa do int64 é o que torna a verificação
// acima inalcançável por Parse. Se alguém aumentar o limite de entrada, este
// teste avisa que a verificação deixou de ser defesa em profundidade e passou a
// ser a única defesa.
func TestLimiteDeEntradaMantemAParteInteiraDentroDoInt64(t *testing.T) {
	// maxInputLen cobre: parte inteira + '.' + Scale casas.
	maxDigitosInteiros := maxInputLen - 1 - Scale

	maiorPossivel := strings.Repeat("9", maxDigitosInteiros)
	if _, err := parseDigits(maiorPossivel); err != nil {
		t.Fatalf("a maior parte inteira aceitável (%d dígitos) não deveria estourar sozinha: %v",
			maxDigitosInteiros, err)
	}
}
