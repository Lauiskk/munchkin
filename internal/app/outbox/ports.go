// Package outbox publica os eventos que a transação de negócio gravou.
//
// A separação é o ponto: nada aqui roda dentro da transação que originou o
// evento. Quem grava é o caso de uso financeiro, no mesmo commit do saldo e do
// ledger; quem publica é este pacote, depois, sem saber que aquele commit
// existiu. É essa fronteira que torna "publicação anterior ao commit"
// impossível por construção, e não por disciplina.
package outbox

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Lauiskk/munchkin/internal/domain/event"
)

// PendingEvent é uma linha da outbox pronta para publicação.
//
// O payload vem como JSON opaco porque é assim que ele foi gravado: um
// instantâneo imutável do momento do commit. Reconstituir o objeto tipado aqui
// permitiria que uma mudança futura na struct alterasse um evento passado —
// exatamente o que o instantâneo existe para impedir.
type PendingEvent struct {
	ID            event.ID
	Type          event.Type
	AggregateType event.AggregateType
	AggregateID   string
	Version       int
	CorrelationID string
	CausationID   string
	Payload       json.RawMessage
	OccurredAt    time.Time
	Attempts      int
}

// Repository é o que o publicador precisa da persistência.
type Repository interface {
	// Claim reivindica até `limite` eventos pendentes e devidos, marcando-os
	// com um lease com a duração indicada.
	//
	// É uma única instrução: a atomicidade é do banco. Nenhuma transação fica
	// aberta enquanto a publicação acontece — segurar uma transação por uma
	// chamada de rede é como filas em banco viram incidente.
	//
	// Recebe DURAÇÃO, não instante, de propósito. O lease é comparado com o
	// relógio do banco por todas as instâncias; se cada uma gravasse um
	// instante calculado pelo próprio relógio, uma máquina adiantada
	// reivindicaria por mais tempo do que deveria e uma atrasada perderia o
	// próprio trabalho para as outras. O único relógio que todas compartilham
	// é o do PostgreSQL.
	Claim(ctx context.Context, dono string, lease time.Duration, limite int) ([]PendingEvent, error)

	// MarkPublished conclui o evento.
	//
	// A condição é o identificador, e não o lease. Se o lease expirou enquanto
	// a publicação acontecia, ela ainda assim aconteceu — condicionar a
	// gravação ao lease faria uma publicação real ser esquecida, e o evento
	// sairia de novo sem necessidade.
	MarkPublished(ctx context.Context, id event.ID) error

	// Reschedule devolve o evento para a fila com mais uma tentativa contada,
	// registra a causa e libera o lease. A espera também é relativa ao relógio
	// do banco, pela mesma razão do lease.
	Reschedule(ctx context.Context, id event.ID, espera time.Duration, motivo string) error

	// OldestPendingAge devolve há quanto tempo o evento pendente mais antigo
	// espera. Zero quando não há pendentes.
	//
	// É a medida honesta de atraso da outbox: contar quantos estão pendentes
	// não distingue mil eventos recém-gravados de um único evento parado há uma
	// hora, e é o segundo que indica problema.
	OldestPendingAge(ctx context.Context) (time.Duration, error)
}

// Message é o que vai para o transporte.
type Message struct {
	// DeduplicationID é o eventId. Uma queda entre publicar e confirmar faz o
	// evento sair de novo com o MESMO valor, e a fila FIFO o descarta — é o que
	// converte at-least-once em efeito único no consumidor.
	DeduplicationID string
	// GroupID é o agregado, isto é, a carteira. Ordena os eventos de uma
	// carteira entre si sem serializar carteiras distintas.
	GroupID string
	Body    []byte
}

// Publisher entrega a mensagem ao transporte.
type Publisher interface {
	Publish(ctx context.Context, m Message) error
}
