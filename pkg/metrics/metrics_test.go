package metrics_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/internal/app"
	"github.com/Lauiskk/munchkin/pkg/metrics"
)

// AC-11 — nenhum rótulo pode carregar identificador.
//
// A lista é fechada e conferida por igualdade: um rótulo novo com nome de
// identificador — walletId, transactionId, messageId — quebraria o teste em vez
// de publicar a lista de carteiras para quem lê /metrics, e de multiplicar as
// séries até derrubar o coletor.
func TestNenhumRotuloCarregaIdentificador(t *testing.T) {
	r := metrics.New()

	// Toca cada métrica para que ela apareça na coleta: um vetor sem nenhuma
	// combinação de rótulos não é exportado.
	r.TransactionSettled("BET", "PROCESSED", "", app.SourceHTTP)
	r.IdempotentReplay(app.SourceSQS)
	r.ProcessingDuration("BET", app.SourceHTTP, 10*time.Millisecond)
	r.ConcurrencyConflict("wallet_version")
	r.WorkerRetry("outbox.publisher")
	r.MessageDeadLettered("invalid_message")
	r.ReconciliationDivergence()
	r.OutboxPendingAge(time.Second)

	familias, err := r.Gatherer().Gather()
	require.NoError(t, err)
	require.Len(t, familias, 8, "as oito métricas do §12")

	permitidos := map[string]struct{}{
		"kind": {}, "status": {}, "failure_code": {},
		"source": {}, "worker": {}, "reason": {},
	}

	for _, f := range familias {
		assert.Contains(t, f.GetName(), "munchkin_",
			"toda métrica é prefixada, para não colidir num Prometheus compartilhado")
		for _, m := range f.GetMetric() {
			for _, rotulo := range m.GetLabel() {
				assert.Contains(t, permitidos, rotulo.GetName(),
					"rótulo não permitido em %s", f.GetName())
			}
		}
	}
}

// As oito métricas do §12, pelo nome. Renomear uma quebra painel e alerta de
// quem já coleta, então a lista é explícita.
func TestAsOitoMetricasExigidasExistem(t *testing.T) {
	r := metrics.New()
	r.TransactionSettled("BET", "PROCESSED", "", app.SourceHTTP)
	r.IdempotentReplay(app.SourceHTTP)
	r.ProcessingDuration("BET", app.SourceHTTP, time.Millisecond)
	r.ConcurrencyConflict("wallet_version")
	r.WorkerRetry("outbox.publisher")
	r.MessageDeadLettered("invalid_message")
	r.ReconciliationDivergence()
	r.OutboxPendingAge(0)

	familias, err := r.Gatherer().Gather()
	require.NoError(t, err)

	nomes := make(map[string]struct{}, len(familias))
	for _, f := range familias {
		nomes[f.GetName()] = struct{}{}
	}

	for _, esperado := range []string{
		"munchkin_wager_transactions_total",          // resultados por status
		"munchkin_idempotent_replays_total",          // duplicatas
		"munchkin_worker_retries_total",              // retries
		"munchkin_messages_dead_lettered_total",      // DLQ
		"munchkin_concurrency_conflicts_total",       // conflitos de concorrência
		"munchkin_outbox_pending_age_seconds",        // atraso da outbox
		"munchkin_wager_processing_duration_seconds", // latência de processamento
		"munchkin_reconciliation_divergences_total",  // divergências de reconciliação
	} {
		assert.Contains(t, nomes, esperado)
	}
}

// AC-12
func TestRegistroNuloSatisfazAPortaESeguraNada(t *testing.T) {
	var m app.Metrics = app.NopMetrics{}

	assert.NotPanics(t, func() {
		m.TransactionSettled("BET", "PROCESSED", "", app.SourceHTTP)
		m.IdempotentReplay(app.SourceHTTP)
		m.ProcessingDuration("BET", app.SourceHTTP, time.Second)
		m.ConcurrencyConflict("x")
		m.WorkerRetry("x")
		m.MessageDeadLettered("x")
		m.ReconciliationDivergence()
		m.OutboxPendingAge(time.Second)
	})
}

// TestAsOitoMetricasAparecemAntesDoPrimeiroUso guarda o §12 ao pé da letra.
//
// Contador com rótulo não existe no /metrics até alguém tocar numa combinação.
// Como retentativa e conflito de concorrência não acontecem numa aplicação
// saudável, as duas ficavam invisíveis exatamente quando está tudo bem — e uma
// varredura de um sistema recém-subido mostrava seis das oito que o enunciado
// manda expor.
//
// Para quem opera, "sem dados" e "zero" são afirmações diferentes: alerta sobre
// série ausente não dispara e não se distingue de alerta quebrado.
func TestAsOitoMetricasAparecemAntesDoPrimeiroUso(t *testing.T) {
	r := metrics.New()

	familias, err := r.Gatherer().Gather()
	require.NoError(t, err)

	presentes := map[string]bool{}
	for _, f := range familias {
		presentes[f.GetName()] = true
	}

	// As que PODEM nunca acontecer num sistema saudável, e por isso precisam
	// aparecer em zero. Todas têm conjunto de rótulos fechado e pequeno.
	//
	// Ficam de fora `wager_transactions_total` (cruza quatro rótulos, centenas
	// de combinações) e a duração (histograma, um balde por faixa e por
	// combinação). As duas aparecem na primeira operação processada, que num
	// serviço em uso acontece sempre — a diferença com retentativa e conflito,
	// que podem não acontecer nunca, é o que decide quem entra nesta lista.
	for _, nome := range []string{
		"munchkin_idempotent_replays_total",
		"munchkin_worker_retries_total",
		"munchkin_messages_dead_lettered_total",
		"munchkin_concurrency_conflicts_total",
		"munchkin_reconciliation_divergences_total",
		"munchkin_outbox_pending_age_seconds",
	} {
		assert.True(t, presentes[nome],
			"%s não aparece antes do primeiro uso: quem varrer um sistema "+
				"saudável não vai encontrá-la", nome)
	}
}
