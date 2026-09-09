package app

import "errors"

// ErrNotFound indica recurso inexistente.
//
// Mora na camada de aplicação e não no adaptador porque é o caso de uso e o
// handler que precisam reconhecê-lo. Se vivesse no adaptador de persistência,
// reconhecê-lo obrigaria a camada de aplicação a importá-lo — e a regra de
// negócio passaria a depender do formato do banco.
//
// O adaptador implementa a porta e devolve este erro; a direção da dependência
// fica adaptador → aplicação, que é a correta.
var ErrNotFound = errors.New("recurso não encontrado")
