package app

import "time"

// Metrics é o que os casos de uso registram sobre o que fizeram.
//
// É uma porta, como as de persistência e transporte: a camada de aplicação não
// conhece Prometheus, e não deve conhecer. Trocar o coletor é trocar a
// implementação, e nenhum caso de uso muda.
//
// Os rótulos são categorias FECHADAS — tipo, status, código de falha, origem,
// worker, motivo. Nenhum identificador entra: além de explodir a cardinalidade,
// um `walletId` em rótulo publicaria a lista de carteiras para quem lesse
// /metrics.
type Metrics interface {
	// TransactionSettled registra o desfecho de uma operação. `failureCode` é
	// vazio quando ela foi processada.
	TransactionSettled(kind, status, failureCode, source string)
	// IdempotentReplay registra um reenvio que encontrou resultado persistido.
	IdempotentReplay(source string)
	// ProcessingDuration registra quanto levou o caminho financeiro.
	ProcessingDuration(kind, source string, d time.Duration)
	// ConcurrencyConflict registra uma disputa perdida.
	ConcurrencyConflict(kind string)
	// WorkerRetry registra uma retentativa agendada por um processo de fundo.
	WorkerRetry(worker string)
	// MessageDeadLettered registra uma mensagem descartada para a DLQ.
	MessageDeadLettered(reason string)
	// ReconciliationDivergence registra saldo em desacordo com o ledger.
	ReconciliationDivergence()
	// OutboxPendingAge publica a idade do evento pendente mais antigo.
	OutboxPendingAge(d time.Duration)
}

// Origens possíveis de uma operação. São rótulo, então a lista é fechada.
const (
	SourceHTTP   = "http"
	SourceSQS    = "sqs"
	SourceWorker = "worker"
)

// NopMetrics não registra nada.
//
// Existe para que teste e composição sem observabilidade não precisem de um
// registro Prometheus, e para que a ausência de coletor seja uma decisão
// explícita em vez de um ponteiro nulo esperando para estourar.
type NopMetrics struct{}

func (NopMetrics) TransactionSettled(string, string, string, string) {}
func (NopMetrics) IdempotentReplay(string)                           {}
func (NopMetrics) ProcessingDuration(string, string, time.Duration)  {}
func (NopMetrics) ConcurrencyConflict(string)                        {}
func (NopMetrics) WorkerRetry(string)                                {}
func (NopMetrics) MessageDeadLettered(string)                        {}
func (NopMetrics) ReconciliationDivergence()                         {}
func (NopMetrics) OutboxPendingAge(time.Duration)                    {}

var _ Metrics = NopMetrics{}
