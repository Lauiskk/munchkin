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

// ErrUnavailable indica indisponibilidade TRANSITÓRIA de uma dependência.
//
// Existe para separar "tente de novo" de "há um defeito aqui". Sem essa
// distinção, um banco que caiu por trinta segundos aparece para quem integra
// como erro interno da aplicação — e um erro interno não se retenta, se reporta.
// O §9 do enunciado exige que indisponibilidade transitória seja distinguível
// pelo contrato, e é este erro que a torna distinguível.
var ErrUnavailable = errors.New("dependência temporariamente indisponível")
