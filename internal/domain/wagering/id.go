package wagering

import (
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// ErrInvalidID indica identificador ausente ou malformado.
var ErrInvalidID = errors.New("identificador inválido")

// maxExternalIDLen limita os identificadores vindos do provedor.
//
// Eles entram em índice, em log e em mensagem de erro. Sem limite, um provedor
// determina o tamanho do que gravamos e do que registramos.
const maxExternalIDLen = 128

// TransactionID identifica uma transação no nosso sistema.
type TransactionID uuid.UUID

// NewTransactionID gera um identificador de transação.
func NewTransactionID() (TransactionID, error) {
	u, err := uuid.NewV7()
	if err != nil {
		return TransactionID{}, fmt.Errorf("geração de identificador de transação: %w", err)
	}
	return TransactionID(u), nil
}

// ParseTransactionID converte a representação textual.
func ParseTransactionID(s string) (TransactionID, error) {
	u, err := uuid.Parse(s)
	if err != nil || u == uuid.Nil {
		return TransactionID{}, fmt.Errorf("%w: transação %q", ErrInvalidID, s)
	}
	return TransactionID(u), nil
}

func (t TransactionID) String() string { return uuid.UUID(t).String() }
func (t TransactionID) IsZero() bool   { return t == TransactionID{} }

// ProviderID identifica o provedor de jogos. Vem do token, nunca do corpo.
type ProviderID string

// ExternalID é o identificador que o provedor dá à operação no sistema dele.
type ExternalID string

// RoundID identifica a rodada.
type RoundID string

// GameID identifica o jogo.
type GameID string

// ParseProviderID valida o identificador de provedor.
func ParseProviderID(s string) (ProviderID, error) {
	if err := validarIdentificadorExterno("provedor", s); err != nil {
		return "", err
	}
	return ProviderID(s), nil
}

// ParseExternalID valida o identificador externo da operação.
func ParseExternalID(s string) (ExternalID, error) {
	if err := validarIdentificadorExterno("transação externa", s); err != nil {
		return "", err
	}
	return ExternalID(s), nil
}

// ParseRoundID valida o identificador de rodada.
func ParseRoundID(s string) (RoundID, error) {
	if err := validarIdentificadorExterno("rodada", s); err != nil {
		return "", err
	}
	return RoundID(s), nil
}

// ParseGameID valida o identificador de jogo.
func ParseGameID(s string) (GameID, error) {
	if err := validarIdentificadorExterno("jogo", s); err != nil {
		return "", err
	}
	return GameID(s), nil
}

// validarIdentificadorExterno aplica as regras comuns aos identificadores que
// vêm de fora.
//
// O conjunto de caracteres é restrito de propósito. Esses valores acabam em log
// estruturado e em mensagem de erro; aceitar quebra de linha ou aspas permitiria
// forjar registros. Aceitar espaço permitiria que " tx-1" e "tx-1" fossem
// operações distintas para o índice de idempotência e a mesma para quem lê.
func validarIdentificadorExterno(nome, valor string) error {
	if valor == "" {
		return fmt.Errorf("%w: %s ausente", ErrInvalidID, nome)
	}
	if len(valor) > maxExternalIDLen {
		return fmt.Errorf("%w: %s com %d caracteres excede o máximo de %d",
			ErrInvalidID, nome, len(valor), maxExternalIDLen)
	}
	if strings.TrimSpace(valor) != valor {
		return fmt.Errorf("%w: %s não pode ter espaço nas bordas", ErrInvalidID, nome)
	}
	for _, r := range valor {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == ':', r == '.':
		default:
			return fmt.Errorf("%w: %s contém caractere não permitido (%q)",
				ErrInvalidID, nome, string(r))
		}
	}
	return nil
}

func (p ProviderID) String() string { return string(p) }
func (e ExternalID) String() string { return string(e) }
func (r RoundID) String() string    { return string(r) }
func (g GameID) String() string     { return string(g) }

// MarshalText serializa como texto.
//
// Necessário: TransactionID é definido sobre uuid.UUID, que é [16]byte. Tipos
// definidos não herdam métodos, então sem isto o identificador sairia como
// array de números no JSON — ilegível para o consumidor e inútil para
// correlacionar.
func (i TransactionID) MarshalText() ([]byte, error) { return []byte(i.String()), nil }

// UnmarshalText lê a representação textual, validando.
func (i *TransactionID) UnmarshalText(data []byte) error {
	parsed, err := ParseTransactionID(string(data))
	if err != nil {
		return err
	}
	*i = parsed
	return nil
}
