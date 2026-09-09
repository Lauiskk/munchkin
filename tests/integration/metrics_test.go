//go:build integration

package integration_test

import (
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/internal/app"
	appwagering "github.com/Lauiskk/munchkin/internal/app/wagering"
	domain "github.com/Lauiskk/munchkin/internal/domain/wagering"
)

// registroDeMetricas anota o que foi medido, para que o teste cobre a
// instrumentação como cobra qualquer outro comportamento.
//
// Uma métrica declarada e nunca incrementada passa despercebida: ela aparece em
// /metrics com valor zero, indistinguível de "nada aconteceu ainda". O dublê
// existe para que o caminho real seja quem a incrementa, e não um teste que
// chama o coletor à mão.
type registroDeMetricas struct {
	mu sync.Mutex

	desfechos    []string
	replays      []string
	duracoes     []string
	conflitos    []string
	retentativas []string
	deadLetters  []string
	divergencias int
	atrasoOutbox time.Duration
}

func (m *registroDeMetricas) TransactionSettled(kind, status, failureCode, source string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.desfechos = append(m.desfechos, kind+"/"+status+"/"+failureCode+"/"+source)
}

func (m *registroDeMetricas) IdempotentReplay(source string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.replays = append(m.replays, source)
}

func (m *registroDeMetricas) ProcessingDuration(kind, source string, _ time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.duracoes = append(m.duracoes, kind+"/"+source)
}

func (m *registroDeMetricas) ConcurrencyConflict(kind string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.conflitos = append(m.conflitos, kind)
}

func (m *registroDeMetricas) WorkerRetry(worker string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retentativas = append(m.retentativas, worker)
}

func (m *registroDeMetricas) MessageDeadLettered(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deadLetters = append(m.deadLetters, reason)
}

func (m *registroDeMetricas) ReconciliationDivergence() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.divergencias++
}

func (m *registroDeMetricas) OutboxPendingAge(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.atrasoOutbox = d
}

func (m *registroDeMetricas) lido() registroDeMetricas {
	m.mu.Lock()
	defer m.mu.Unlock()
	return registroDeMetricas{
		desfechos:    append([]string(nil), m.desfechos...),
		replays:      append([]string(nil), m.replays...),
		duracoes:     append([]string(nil), m.duracoes...),
		conflitos:    append([]string(nil), m.conflitos...),
		retentativas: append([]string(nil), m.retentativas...),
		deadLetters:  append([]string(nil), m.deadLetters...),
		divergencias: m.divergencias,
		atrasoOutbox: m.atrasoOutbox,
	}
}

var _ app.Metrics = (*registroDeMetricas)(nil)

// AC-1, AC-2, AC-3, AC-4 — o caminho financeiro é quem instrumenta.
func TestOperacoesAlimentamAsMetricas(t *testing.T) {
	a := novoAmbienteOperacao(t)
	w, p := a.carteira(t, "100.00")

	entrada := operacao(t, w, p, "tx-1", domain.Bet, "30.00")
	_, err := a.processor.Process(a.ctx, entrada)
	require.NoError(t, err)

	// Reenvio da mesma operação: é duplicata, não desfecho novo.
	_, err = a.processor.Process(a.ctx, entrada)
	require.NoError(t, err)

	// Recusa de negócio: o código de falha entra como rótulo.
	_, err = a.processor.Process(a.ctx, operacao(t, w, p, "tx-2", domain.Bet, "999.00"))
	require.NoError(t, err)

	m := a.metricas.lido()
	assert.Equal(t, []string{"BET/PROCESSED//http", "BET/REJECTED/INSUFFICIENT_FUNDS/http"},
		m.desfechos, "o replay não pode contar como desfecho novo")
	assert.Equal(t, []string{"http"}, m.replays)
	assert.Equal(t, []string{"BET/http", "BET/http"}, m.duracoes,
		"latência é medida do trabalho, e a recusa também é trabalho")
}

// AC-1 com a outra origem: a mesma operação por fila é contada como fila.
func TestOrigemDaOperacaoChegaNaMetrica(t *testing.T) {
	a := novoAmbienteInbox(t)
	w, p := a.carteira(t, "100.00")

	entrada := operacao(t, w, p, "tx-1", domain.Bet, "30.00")
	entrada.Source = app.SourceSQS
	_, _, err := a.handler.Handle(a.ctx, mensagem("msg-1", "h1", entrada))
	require.NoError(t, err)

	assert.Equal(t, []string{"BET/PROCESSED//sqs"}, a.metricas.lido().desfechos)
}

// AC-5 — a retentativa do worker é contada quando ele reagenda.
func TestRetentativaDoWorkerEhContada(t *testing.T) {
	a := novoAmbienteReversao(t)
	w, p := a.carteira(t, "100.00")

	_, err := a.processor.Process(a.ctx,
		reversao(t, w, p, "rf-1", domain.Refund, "40.00", "nunca-chega"))
	require.NoError(t, err)

	a.relogio.avancar(appwagering.Backoff(1) + time.Second)
	_, err = a.resolver.RunOnce(a.ctx)
	require.NoError(t, err)

	assert.Contains(t, a.metricas.lido().retentativas, "reference.resolver")
}

// AC-6 — o rótulo do descarte é a categoria, não a mensagem de erro.
func TestDescarteParaDLQEhContadoPorCategoria(t *testing.T) {
	a := novoAmbienteConsumo(t)
	w, p := a.carteira(t, "100.00")

	a.publicar(t, "d1", `{"messageId":"m1","type":"Desconhecido","data":{}}`)
	corpo := mensagemDeAposta("msg-1", "tx-1", w, p, "BET", "30.00")
	a.publicar(t, "d2", corpo)
	_, err := a.consumer.RunOnce(a.ctx)
	require.NoError(t, err)

	a.publicar(t, "d3", mensagemDeAposta("msg-1", "tx-outra", w, p, "BET", "10.00"))
	_, err = a.consumer.RunOnce(a.ctx)
	require.NoError(t, err)

	m := a.metricas.lido()
	assert.Contains(t, m.deadLetters, "invalid_message")
	assert.Contains(t, m.deadLetters, "hash_mismatch",
		"a categoria distingue conteúdo inválido de reentrega divergente")
	for _, r := range m.deadLetters {
		assert.NotContains(t, r, "msg-1",
			"rótulo não pode carregar identificador: cada valor viraria uma série")
	}
}

// AC-8
func TestDivergenciaDeReconciliacaoEhContada(t *testing.T) {
	a := novoAmbienteExtrato(t)
	w, p := a.carteira(t, "100.00")
	a.apostas(t, w, p, 2)

	_, err := a.reconciler.Run(a.ctx, w)
	require.NoError(t, err)
	require.Zero(t, a.metricas.lido().divergencias, "conferência consistente não conta nada")

	require.NoError(t, a.db.Session(a.ctx).Exec(
		`UPDATE wallets SET balance_minor = 1 WHERE id = ?`, uuid.UUID(w)).Error)
	_, err = a.reconciler.Run(a.ctx, w)
	require.NoError(t, err)

	assert.Equal(t, 1, a.metricas.lido().divergencias)
}

// AC-9 e AC-E1
func TestAtrasoDaOutboxRefleteOPendenteMaisAntigo(t *testing.T) {
	a := novoAmbienteOutbox(t)
	a.carteira(t, "100.00") // dois eventos pendentes

	_, err := a.dispatcher.RunOnce(a.ctx)
	require.NoError(t, err)
	comPendentes := a.metricas.lido().atrasoOutbox

	// Nada pendente: o medidor tem de VOLTAR a zero, e não ficar preso no
	// último valor alto — um medidor que só sobe é um alarme permanente.
	_, err = a.dispatcher.RunOnce(a.ctx)
	require.NoError(t, err)

	assert.Positive(t, comPendentes, "havia evento esperando quando a rodada começou")
	assert.Zero(t, a.metricas.lido().atrasoOutbox)
}
