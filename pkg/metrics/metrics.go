// Package metrics implementa a porta de observabilidade sobre Prometheus.
//
// Mora em pkg/ e não em internal/adapter/ porque não adapta nada do domínio:
// é infraestrutura de processo, do mesmo tipo de `logs` e `safe`.
//
// Coleta e exposição são coisas separadas: quem serve /metrics é o pacote `ops`,
// junto da documentação. Aqui só se define e se registra o que é medido.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/fx"

	"github.com/Lauiskk/munchkin/internal/app"
)

// namespace prefixa toda métrica desta aplicação. Num Prometheus compartilhado,
// é o que impede colisão com métrica de outro serviço com nome parecido.
const namespace = "munchkin"

// Registry reúne as métricas do §12 do enunciado.
type Registry struct {
	registro *prometheus.Registry

	transacoes   *prometheus.CounterVec
	replays      *prometheus.CounterVec
	duracao      *prometheus.HistogramVec
	retentativas *prometheus.CounterVec
	deadLetter   *prometheus.CounterVec
	conflitos    *prometheus.CounterVec
	divergencias prometheus.Counter
	atrasoOutbox prometheus.Gauge
}

// New monta o registro com as oito métricas.
//
// Registro próprio, e não o global: o global recolhe o que qualquer biblioteca
// importada resolver registrar, e o conteúdo de /metrics deixaria de ser uma
// decisão para virar consequência de uma lista de dependências.
func New() *Registry {
	r := &Registry{
		registro: prometheus.NewRegistry(),

		transacoes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "wager_transactions_total",
			Help:      "Operações financeiras por tipo, desfecho, código de falha e origem.",
		}, []string{"kind", "status", "failure_code", "source"}),

		replays: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "idempotent_replays_total",
			Help:      "Reenvios que encontraram resultado já persistido.",
		}, []string{"source"}),

		duracao: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "wager_processing_duration_seconds",
			Help:      "Duração do caminho financeiro, do início da transação ao desfecho.",
			// Faixas ajustadas ao que se espera: uma operação toca uma linha
			// travada e commita. Os padrões da biblioteca começam em 5ms e
			// perderiam a resolução justamente onde ela importa.
			Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		}, []string{"kind", "source"}),

		retentativas: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "worker_retries_total",
			Help:      "Retentativas agendadas por processos de fundo.",
		}, []string{"worker"}),

		deadLetter: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "messages_dead_lettered_total",
			Help:      "Mensagens descartadas para a DLQ, por motivo.",
		}, []string{"reason"}),

		conflitos: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "concurrency_conflicts_total",
			Help:      "Disputas perdidas por concorrência, por tipo de conflito.",
		}, []string{"kind"}),

		divergencias: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "reconciliation_divergences_total",
			Help:      "Conferências em que o saldo discordou do ledger.",
		}),

		atrasoOutbox: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "outbox_pending_age_seconds",
			Help:      "Idade do evento pendente mais antigo na outbox.",
		}),
	}

	r.registro.MustRegister(
		r.transacoes, r.replays, r.duracao, r.retentativas,
		r.deadLetter, r.conflitos, r.divergencias, r.atrasoOutbox,
	)

	// Contador com rótulo não existe no /metrics até alguém tocar numa
	// combinação. O §12 manda EXPOR métrica de retentativa e de conflito de
	// concorrência, e numa aplicação saudável nenhuma das duas acontece — então
	// as duas ficavam invisíveis justamente quando está tudo bem.
	//
	// Para quem opera, isso é pior que parecer: "sem dados" e "zero" são
	// afirmações diferentes. Um alerta sobre série ausente não dispara e não se
	// distingue de um alerta quebrado; sobre uma série em zero, dispara.
	//
	// Os valores de rótulo são fechados e conhecidos, então dá para declará-los
	// aqui. O conflito por versão de carteira é, por desenho, inalcançável —
	// e é exatamente por isso que ele precisa aparecer em zero: é o sintoma de
	// que alguma escrita escapou do caminho travado, e sintoma que não se
	// observa não serve de sintoma.
	for _, worker := range []string{"outbox.publisher", "reference.resolver"} {
		r.retentativas.WithLabelValues(worker)
	}
	for _, conflito := range []string{"pending_reference", "wallet_version"} {
		r.conflitos.WithLabelValues(conflito)
	}
	for _, motivo := range []string{"invalid_message", "hash_mismatch"} {
		r.deadLetter.WithLabelValues(motivo)
	}
	for _, origem := range []string{"http", "sqs", "worker"} {
		r.replays.WithLabelValues(origem)
	}
	return r
}

// Duas ficam DE FORA desta pré-declaração, e é decisão e não esquecimento.
//
// `wager_transactions_total` cruza tipo, estado, código de falha e origem: são
// centenas de combinações, e a esmagadora maioria nunca acontece. Declarar todas
// trocaria um problema por outro pior — invisibilidade por uma explosão de
// séries que este mesmo pacote tem teste para impedir.
//
// `wager_processing_duration_seconds` é histograma: cada combinação de rótulo
// custa um balde por faixa, e o preço de pré-declarar cresce depressa.
//
// As duas aparecem no primeiro uso, e o primeiro uso é a primeira operação que
// o serviço processa — diferente de retentativa e conflito, que podem não
// acontecer nunca. É essa diferença que decide quem entra aqui.

// Gatherer expõe o registro para quem serve /metrics.
func (r *Registry) Gatherer() prometheus.Gatherer { return r.registro }

func (r *Registry) TransactionSettled(kind, status, failureCode, source string) {
	r.transacoes.WithLabelValues(kind, status, failureCode, source).Inc()
}

func (r *Registry) IdempotentReplay(source string) {
	r.replays.WithLabelValues(source).Inc()
}

func (r *Registry) ProcessingDuration(kind, source string, d time.Duration) {
	r.duracao.WithLabelValues(kind, source).Observe(d.Seconds())
}

func (r *Registry) ConcurrencyConflict(kind string) {
	r.conflitos.WithLabelValues(kind).Inc()
}

func (r *Registry) WorkerRetry(worker string) {
	r.retentativas.WithLabelValues(worker).Inc()
}

func (r *Registry) MessageDeadLettered(reason string) {
	r.deadLetter.WithLabelValues(reason).Inc()
}

func (r *Registry) ReconciliationDivergence() { r.divergencias.Inc() }

func (r *Registry) OutboxPendingAge(d time.Duration) { r.atrasoOutbox.Set(d.Seconds()) }

var _ app.Metrics = (*Registry)(nil)

// Module provê o registro e a porta que os casos de uso enxergam.
var Module = fx.Module("metrics",
	fx.Provide(
		New,
		func(r *Registry) app.Metrics { return r },
	),
)
