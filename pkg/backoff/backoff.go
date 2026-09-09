// Package backoff calcula esperas exponenciais entre tentativas.
package backoff

import "time"

// maxDeslocamento limita o deslocamento antes de aplicá-lo: deslocar um int64
// além de algumas dezenas de posições não produz resultado útil, e o teto já
// teria sido alcançado muito antes disso.
const maxDeslocamento = 20

// Exponential devolve a espera da tentativa indicada, dobrando a cada uma até
// o teto. A primeira tentativa espera `base`.
//
// O crescimento é por deslocamento de bits, não por exponenciação em ponto
// flutuante. A primeira versão disto usava math.Pow e o gate de float a
// recusou — com razão: o pacote estava no caminho do dinheiro, e a regra não
// abre exceção para "mas aqui não é valor monetário". A versão inteira, além de
// passar, é exata e não tem o que arredondar.
func Exponential(tentativa int, base, teto time.Duration) time.Duration {
	if tentativa < 1 {
		tentativa = 1
	}

	deslocamento := tentativa - 1
	if deslocamento > maxDeslocamento {
		return teto
	}

	espera := base << deslocamento
	// A comparação com zero pega o estouro do int64, que aparece como valor
	// negativo — sem ela, uma tentativa alta devolveria espera no passado.
	if espera > teto || espera <= 0 {
		return teto
	}
	return espera
}
