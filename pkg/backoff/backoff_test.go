package backoff_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/Lauiskk/munchkin/pkg/backoff"
)

func TestExponentialDobraAteOTeto(t *testing.T) {
	const (
		base = 5 * time.Second
		teto = 5 * time.Minute
	)

	casos := map[int]time.Duration{
		0:  base, // tentativa inválida é tratada como a primeira
		1:  base,
		2:  10 * time.Second,
		3:  20 * time.Second,
		4:  40 * time.Second,
		5:  80 * time.Second,
		6:  160 * time.Second,
		7:  teto, // 320s passaria de 5min
		50: teto,
	}

	for tentativa, esperado := range casos {
		assert.Equal(t, esperado, backoff.Exponential(tentativa, base, teto),
			"tentativa %d", tentativa)
	}
}

// Uma tentativa alta o bastante estoura o int64 no deslocamento, e o resultado
// aparece negativo. Espera negativa agendaria a próxima tentativa no passado, o
// que transformaria o backoff num laço apertado — exatamente o oposto do que
// ele existe para fazer.
func TestExponentialNuncaDevolveEsperaNaoPositiva(t *testing.T) {
	for _, tentativa := range []int{1, 10, 62, 63, 64, 1000, 1 << 20} {
		espera := backoff.Exponential(tentativa, time.Second, time.Minute)
		assert.Positive(t, espera, "tentativa %d", tentativa)
		assert.LessOrEqual(t, espera, time.Minute, "tentativa %d", tentativa)
	}
}
