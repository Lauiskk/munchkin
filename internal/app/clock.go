// Package app reúne o que é comum aos casos de uso.
package app

import "time"

// Clock fornece o instante atual.
//
// É interface para que o teste controle o tempo. Chamar time.Now() direto no
// caso de uso torna impossível verificar, por exemplo, que dois registros
// gravados na mesma operação carregam o mesmo instante.
type Clock interface {
	Now() time.Time
}

// SystemClock lê o relógio do sistema, sempre em UTC.
type SystemClock struct{}

// Now devolve o instante atual em UTC.
//
// Normalizar aqui, e não em cada chamador, é o que impede que o fuso da máquina
// entre nos dados — duas instâncias em fusos diferentes gravariam instantes
// incomparáveis para a mesma operação.
func (SystemClock) Now() time.Time { return time.Now().UTC() }
