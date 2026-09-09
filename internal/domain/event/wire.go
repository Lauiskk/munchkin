package event

import (
	"encoding/json"
	"time"
)

// timestampLayout é o formato dos instantes no transporte: RFC 3339, em UTC,
// com milissegundos.
//
// A precisão fixa é deliberada. `time.Time` serializado pelo padrão da
// biblioteca omite zeros à direita, então o mesmo instante pode sair como
// `...:00Z` ou `...:00.120Z` conforme o valor — e um contrato de integração
// com forma variável é um contrato que alguém vai parsear errado.
const timestampLayout = "2006-01-02T15:04:05.000Z07:00"

// FormatTimestamp normaliza um instante para o formato do contrato.
func FormatTimestamp(t time.Time) string { return t.UTC().Format(timestampLayout) }

// Timestamp é um instante dentro do payload de um evento.
//
// Existe para que os instantes do `data` sigam a mesma forma do `occurredAt` do
// envelope. Um contrato onde um campo sai com milissegundos e outro com
// nanossegundos convida o consumidor a escrever dois parsers, e a errar um.
type Timestamp time.Time

// MarshalJSON grava no formato do contrato.
func (t Timestamp) MarshalJSON() ([]byte, error) {
	return json.Marshal(FormatTimestamp(time.Time(t)))
}

// UnmarshalJSON aceita qualquer precisão fracionária de RFC 3339, e não só a
// que nós emitimos: um evento gravado por uma versão anterior continua legível.
func (t *Timestamp) UnmarshalJSON(dados []byte) error {
	var s string
	if err := json.Unmarshal(dados, &s); err != nil {
		return err
	}
	lido, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return err
	}
	*t = Timestamp(lido.UTC())
	return nil
}

// Time devolve o instante embrulhado.
func (t Timestamp) Time() time.Time { return time.Time(t) }

// Wire é a forma do evento no transporte.
//
// Não é a `Envelope`: a Envelope carrega o payload TIPADO, e existe no momento
// em que o evento é criado. Esta forma é montada na publicação, a partir do que
// foi persistido — e o payload já é JSON opaco ali, porque a outbox guarda um
// instantâneo imutável, não um objeto vivo.
//
// Os campos são exatamente os que o §11 do enunciado exige, mais o
// `aggregateType`, que evita que o consumidor tenha de inferir o tipo do
// agregado a partir do tipo do evento.
type Wire struct {
	EventID       ID              `json:"eventId"`
	EventType     Type            `json:"eventType"`
	AggregateType AggregateType   `json:"aggregateType"`
	AggregateID   string          `json:"aggregateId"`
	CorrelationID string          `json:"correlationId"`
	CausationID   *string         `json:"causationId"`
	OccurredAt    string          `json:"occurredAt"`
	Version       int             `json:"version"`
	Data          json.RawMessage `json:"data"`
}
