package wallet

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// ErrInvalidID indica identificador ausente ou malformado.
var ErrInvalidID = errors.New("identificador inválido")

// ID identifica uma carteira.
//
// É um tipo próprio, e não uma string ou um uuid.UUID cru, porque numa chamada
// com identificador de carteira, de jogador e de transação lado a lado, trocar
// dois deles de posição compila sem reclamar — e o defeito só aparece com o
// dinheiro no lugar errado.
type ID uuid.UUID

// PlayerID identifica um jogador.
type PlayerID uuid.UUID

// NewID gera um identificador de carteira.
//
// Usa UUID versão 7, que é ordenável por tempo: registros criados em sequência
// ficam próximos no índice, o que importa numa tabela que só cresce.
func NewID() (ID, error) {
	u, err := uuid.NewV7()
	if err != nil {
		return ID{}, fmt.Errorf("geração de identificador de carteira: %w", err)
	}
	return ID(u), nil
}

// ParseID converte a representação textual.
func ParseID(s string) (ID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return ID{}, fmt.Errorf("%w: carteira %q", ErrInvalidID, s)
	}
	if u == uuid.Nil {
		return ID{}, fmt.Errorf("%w: carteira nula", ErrInvalidID)
	}
	return ID(u), nil
}

func (i ID) String() string { return uuid.UUID(i).String() }
func (i ID) IsZero() bool   { return i == ID{} }

// NewPlayerID gera um identificador de jogador.
func NewPlayerID() (PlayerID, error) {
	u, err := uuid.NewV7()
	if err != nil {
		return PlayerID{}, fmt.Errorf("geração de identificador de jogador: %w", err)
	}
	return PlayerID(u), nil
}

// ParsePlayerID converte a representação textual.
func ParsePlayerID(s string) (PlayerID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return PlayerID{}, fmt.Errorf("%w: jogador %q", ErrInvalidID, s)
	}
	if u == uuid.Nil {
		return PlayerID{}, fmt.Errorf("%w: jogador nulo", ErrInvalidID)
	}
	return PlayerID(u), nil
}

func (p PlayerID) String() string { return uuid.UUID(p).String() }
func (p PlayerID) IsZero() bool   { return p == PlayerID{} }

// MarshalText serializa como texto.
//
// Necessário: ID é definido sobre uuid.UUID, que é [16]byte. Tipos
// definidos não herdam métodos, então sem isto o identificador sairia como
// array de números no JSON — ilegível para o consumidor e inútil para
// correlacionar.
func (i ID) MarshalText() ([]byte, error) { return []byte(i.String()), nil }

// UnmarshalText lê a representação textual, validando.
func (i *ID) UnmarshalText(data []byte) error {
	parsed, err := ParseID(string(data))
	if err != nil {
		return err
	}
	*i = parsed
	return nil
}

// MarshalText serializa como texto.
//
// Necessário: PlayerID é definido sobre uuid.UUID, que é [16]byte. Tipos
// definidos não herdam métodos, então sem isto o identificador sairia como
// array de números no JSON — ilegível para o consumidor e inútil para
// correlacionar.
func (p PlayerID) MarshalText() ([]byte, error) { return []byte(p.String()), nil }

// UnmarshalText lê a representação textual, validando.
func (p *PlayerID) UnmarshalText(data []byte) error {
	parsed, err := ParsePlayerID(string(data))
	if err != nil {
		return err
	}
	*p = parsed
	return nil
}
