// Package event define os eventos de integração emitidos pelo domínio.
//
// O envelope é comum a todos; o conteúdo é tipado por evento. Tipo e versão não
// são parâmetros do envelope: cada payload os declara, então não existe a
// possibilidade de um evento ser publicado com o tipo de outro.
package event

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Lauiskk/munchkin/internal/domain/wallet"
)

var (
	// ErrInvalidEnvelope indica envelope incompleto.
	ErrInvalidEnvelope = errors.New("envelope de evento inválido")
	// ErrInvalidID indica identificador de evento ausente.
	ErrInvalidID = errors.New("identificador de evento inválido")
)

// ID identifica um evento.
//
// É estável e sobrevive a republicação: uma queda entre publicar e confirmar faz
// o evento sair de novo com o MESMO identificador, e é por ele que o consumidor
// deduplica. Sem essa estabilidade, at-least-once viraria duplicata de verdade.
type ID uuid.UUID

// NewID gera um identificador de evento.
func NewID() (ID, error) {
	u, err := uuid.NewV7()
	if err != nil {
		return ID{}, fmt.Errorf("geração de identificador de evento: %w", err)
	}
	return ID(u), nil
}

// ParseID converte a representação textual.
func ParseID(s string) (ID, error) {
	u, err := uuid.Parse(s)
	if err != nil || u == uuid.Nil {
		return ID{}, fmt.Errorf("%w: %q", ErrInvalidID, s)
	}
	return ID(u), nil
}

func (i ID) String() string { return uuid.UUID(i).String() }
func (i ID) IsZero() bool   { return i == ID{} }

// Type é o nome do evento. Contrato público: uma vez publicado, não muda.
type Type string

// AggregateType nomeia o agregado ao qual o evento pertence.
type AggregateType string

// AggregateWallet é o único agregado publicado hoje.
//
// Os quatro eventos são sobre o que aconteceu com uma carteira, inclusive os que
// descrevem uma transação: uma aposta recusada é um fato sobre aquela carteira.
// Usar a carteira como agregado também dá a chave de particionamento que a fila
// FIFO precisa — eventos da mesma carteira chegam em ordem, e carteiras
// distintas seguem em paralelo.
const AggregateWallet AggregateType = "wallet"

// Payload é o conteúdo tipado de um evento.
//
// Cada implementação declara o próprio tipo e a própria versão. É o que impede
// que o chamador escolha — e erre — o tipo do evento que está publicando.
type Payload interface {
	// EventType devolve o nome do evento.
	EventType() Type
	// EventVersion devolve a versão do contrato deste evento.
	EventVersion() int
	// AggregateID devolve a carteira à qual o evento pertence.
	AggregateID() wallet.ID
}

// Envelope é o evento pronto para ser gravado na outbox.
//
// Os campos são não exportados: o envelope é um instantâneo imutável, e o §11
// exige exatamente isso. O banco reforça por gatilho.
type Envelope struct {
	id            ID
	eventType     Type
	aggregateType AggregateType
	aggregateID   wallet.ID
	correlationID string
	causationID   string
	occurredAt    time.Time
	version       int
	payload       Payload
}

// New monta o envelope a partir do payload.
//
// O tipo, a versão e o agregado vêm do próprio payload. O chamador informa
// apenas o que é do contexto da operação: identidade do evento, correlação,
// causação e instante.
func New(id ID, payload Payload, correlationID, causationID string, occurredAt time.Time) (Envelope, error) {
	if id.IsZero() {
		return Envelope{}, fmt.Errorf("%w: identificador ausente", ErrInvalidID)
	}
	if payload == nil {
		return Envelope{}, fmt.Errorf("%w: conteúdo ausente", ErrInvalidEnvelope)
	}
	if payload.EventType() == "" {
		return Envelope{}, fmt.Errorf("%w: tipo ausente", ErrInvalidEnvelope)
	}
	if payload.EventVersion() < 1 {
		return Envelope{}, fmt.Errorf("%w: versão %d inválida", ErrInvalidEnvelope, payload.EventVersion())
	}
	if payload.AggregateID().IsZero() {
		return Envelope{}, fmt.Errorf("%w: agregado ausente", ErrInvalidEnvelope)
	}

	return Envelope{
		id:            id,
		eventType:     payload.EventType(),
		aggregateType: AggregateWallet,
		aggregateID:   payload.AggregateID(),
		correlationID: correlationID,
		causationID:   causationID,
		occurredAt:    occurredAt.UTC(),
		version:       payload.EventVersion(),
		payload:       payload,
	}, nil
}

// Acessores.

func (e Envelope) ID() ID                       { return e.id }
func (e Envelope) Type() Type                   { return e.eventType }
func (e Envelope) AggregateType() AggregateType { return e.aggregateType }
func (e Envelope) AggregateID() wallet.ID       { return e.aggregateID }
func (e Envelope) CorrelationID() string        { return e.correlationID }
func (e Envelope) CausationID() string          { return e.causationID }
func (e Envelope) OccurredAt() time.Time        { return e.occurredAt }
func (e Envelope) Version() int                 { return e.version }
func (e Envelope) Payload() Payload             { return e.payload }

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
