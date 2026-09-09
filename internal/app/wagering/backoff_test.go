package wagering_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	appwagering "github.com/Lauiskk/munchkin/internal/app/wagering"
)

// AC-E5: o backoff cresce e respeita um teto. Sem teto, uma pendência de longa
// duração acabaria com intervalos de horas, e a resolução chegaria muito depois
// de a referência ter aparecido.
func TestBackoffCresceERespeitaOTeto(t *testing.T) {
	esperado := map[int]time.Duration{
		0: 5 * time.Second, // entrada inválida é tratada como a primeira
		1: 5 * time.Second,
		2: 10 * time.Second,
		3: 20 * time.Second,
		4: 40 * time.Second,
		5: 80 * time.Second,
		6: 160 * time.Second,
	}
	for tentativa, espera := range esperado {
		assert.Equal(t, espera, appwagering.Backoff(tentativa), "tentativa %d", tentativa)
	}

	// A partir de certo ponto, o teto.
	const teto = 5 * time.Minute
	for _, tentativa := range []int{7, 8, 20, 100, 1_000_000} {
		assert.Equal(t, teto, appwagering.Backoff(tentativa),
			"tentativa %d deveria ser limitada pelo teto", tentativa)
	}
}

// O deslocamento precisa ser limitado ANTES de ser aplicado: sem isso, uma
// tentativa grande produziria um valor sem sentido em vez do teto.
func TestBackoffNuncaDevolveValorNaoPositivo(t *testing.T) {
	for _, tentativa := range []int{-5, 0, 1, 30, 62, 63, 64, 1000} {
		assert.Positive(t, appwagering.Backoff(tentativa),
			"tentativa %d devolveu espera não positiva", tentativa)
	}
}
