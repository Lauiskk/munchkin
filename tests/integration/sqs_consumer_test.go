//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/internal/adapter/postgres"
	adaptersqs "github.com/Lauiskk/munchkin/internal/adapter/sqs"
	"github.com/Lauiskk/munchkin/internal/app/inbox"
	"github.com/Lauiskk/munchkin/internal/config"
	domainwallet "github.com/Lauiskk/munchkin/internal/domain/wallet"
)

// filasDeOperacao cria a fila de entrada e a DLQ deste teste.
func filasDeOperacao(t *testing.T) config.AWS {
	t.Helper()

	cfg, err := localstackCompartilhado()
	require.NoError(t, err, "Docker precisa estar em execução para os testes de integração")

	cliente, err := adaptersqs.New(config.Config{AWS: cfg})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	criar := func(nome string) string {
		saida, err := adaptersqs.API(cliente).CreateQueue(ctx, &awssqs.CreateQueueInput{
			QueueName: aws.String(nome),
			Attributes: map[string]string{
				"FifoQueue":                 "true",
				"ContentBasedDeduplication": "false",
			},
		})
		require.NoError(t, err)
		return *saida.QueueUrl
	}

	sufixo := strings.ToLower(uuid.New().String())
	cfg.TransactionsQueueURL = criar("ops-" + sufixo + ".fifo")
	cfg.TransactionsDLQURL = criar("dlq-" + sufixo + ".fifo")
	return cfg
}

type ambienteConsumo struct {
	ambienteOperacao
	cfg      config.AWS
	cliente  *adaptersqs.Client
	consumer *adaptersqs.Consumer
}

func novoAmbienteConsumo(t *testing.T) ambienteConsumo {
	t.Helper()
	a := novoAmbienteOperacao(t)
	cfg := filasDeOperacao(t)

	cliente, err := adaptersqs.New(config.Config{AWS: cfg})
	require.NoError(t, err)

	handler := inbox.NewHandler(a.db, postgres.NewInboxRepository(a.db), a.processor,
		slog.New(slog.NewTextHandler(io.Discard, nil)), consumidorDeTeste)

	return ambienteConsumo{
		ambienteOperacao: a,
		cfg:              cfg,
		cliente:          cliente,
		// Espera zero: o long polling existe para não varrer em vazio em
		// produção; num teste ele só somaria vinte segundos por caso.
		consumer: adaptersqs.NewConsumer(cliente, handler, a.metricas,
			slog.New(slog.NewTextHandler(io.Discard, nil)), 10, 0),
	}
}

// publicar coloca uma mensagem na fila de entrada.
func (a ambienteConsumo) publicar(t *testing.T, dedup, corpo string) {
	t.Helper()
	_, err := adaptersqs.API(a.cliente).SendMessage(a.ctx, &awssqs.SendMessageInput{
		QueueUrl:               aws.String(a.cfg.TransactionsQueueURL),
		MessageBody:            aws.String(corpo),
		MessageGroupId:         aws.String("grupo"),
		MessageDeduplicationId: aws.String(dedup),
	})
	require.NoError(t, err)
}

// naDLQ devolve os motivos das mensagens descartadas.
func (a ambienteConsumo) naDLQ(t *testing.T) []string {
	t.Helper()
	saida, err := adaptersqs.API(a.cliente).ReceiveMessage(a.ctx, &awssqs.ReceiveMessageInput{
		QueueUrl:              aws.String(a.cfg.TransactionsDLQURL),
		MaxNumberOfMessages:   10,
		WaitTimeSeconds:       2,
		VisibilityTimeout:     0,
		MessageAttributeNames: []string{"All"},
	})
	require.NoError(t, err)

	motivos := make([]string, 0, len(saida.Messages))
	for _, m := range saida.Messages {
		motivos = append(motivos, aws.ToString(m.MessageAttributes["reason"].StringValue))
	}
	return motivos
}

func (a ambienteConsumo) restamNaFila(t *testing.T) int {
	t.Helper()
	saida, err := adaptersqs.API(a.cliente).ReceiveMessage(a.ctx, &awssqs.ReceiveMessageInput{
		QueueUrl:            aws.String(a.cfg.TransactionsQueueURL),
		MaxNumberOfMessages: 10,
		WaitTimeSeconds:     2,
		VisibilityTimeout:   0,
	})
	require.NoError(t, err)
	return len(saida.Messages)
}

// mensagemDeAposta monta o envelope do §10.
func mensagemDeAposta(id, externo string, w domainwallet.ID, p domainwallet.PlayerID, kind, valor string) string {
	return fmt.Sprintf(`{"messageId":%q,"type":"WagerTransactionRequested",
	  "occurredAt":"2026-09-09T12:00:00.000Z",
	  "data":{"providerId":"provider-a","externalTransactionId":%q,
	          "idempotencyKey":"provider-a:%s","playerId":%q,"walletId":%q,
	          "roundId":"round-1","gameId":"fortune-chimp","kind":%q,
	          "money":{"amount":%q,"currency":"BRL"}}}`,
		id, externo, externo, p.String(), w.String(), kind, valor)
}

// AC-1 ponta a ponta, com SQS real.
func TestOperacaoEntraPelaFilaESaiDaFilaDepoisDoCommit(t *testing.T) {
	a := novoAmbienteConsumo(t)
	w, p := a.carteira(t, "100.00")
	a.publicar(t, "d1", mensagemDeAposta("msg-1", "tx-1", w, p, "BET", "30.00"))

	tratadas, err := a.consumer.RunOnce(a.ctx)

	require.NoError(t, err)
	assert.Equal(t, 1, tratadas)
	assert.Equal(t, "70.00 BRL", a.saldo(t, w))
	assert.Equal(t, int64(1), a.conta(t, `SELECT count(*) FROM inbox_messages
		WHERE message_id = 'msg-1' AND completed_at IS NOT NULL`))
	assert.Zero(t, a.restamNaFila(t), "a mensagem só sai depois do commit, mas sai")
	assert.Empty(t, a.naDLQ(t))
}

// AC-7 e AC-E2 — erro permanente não gasta as cinco entregas.
func TestEnvelopeInvalidoVaiParaADLQImediatamente(t *testing.T) {
	a := novoAmbienteConsumo(t)
	w, p := a.carteira(t, "100.00")

	a.publicar(t, "d1", `{"messageId":"m1","type":"TipoDesconhecido","data":{}}`)
	a.publicar(t, "d2", `isto não é json`)
	a.publicar(t, "d3", mensagemDeAposta("m3", "tx-3", w, p, "BET", "-5.00"))

	tratadas, err := a.consumer.RunOnce(a.ctx)

	require.NoError(t, err)
	assert.Zero(t, tratadas)
	assert.Len(t, a.naDLQ(t), 3, "retentar não mudaria o resultado: o conteúdo é o mesmo")
	assert.Zero(t, a.restamNaFila(t))
	assert.Equal(t, "100.00 BRL", a.saldo(t, w))
}

// AC-E3 — OPENING é abertura interna e não pode entrar pela fila.
func TestAberturaInternaNaoEntraPelaFila(t *testing.T) {
	a := novoAmbienteConsumo(t)
	w, p := a.carteira(t, "100.00")
	a.publicar(t, "d1", mensagemDeAposta("m1", "tx-1", w, p, "OPENING", "1000.00"))

	_, err := a.consumer.RunOnce(a.ctx)

	require.NoError(t, err)
	assert.Len(t, a.naDLQ(t), 1,
		"um produtor que pudesse enviar OPENING creditaria a própria carteira")
	assert.Equal(t, "100.00 BRL", a.saldo(t, w))
}

// AC-4 pelo transporte: mesma identidade, corpo diferente.
func TestReentregaDivergentePeloTransporteVaiParaADLQ(t *testing.T) {
	a := novoAmbienteConsumo(t)
	w, p := a.carteira(t, "100.00")

	a.publicar(t, "d1", mensagemDeAposta("msg-1", "tx-1", w, p, "BET", "30.00"))
	_, err := a.consumer.RunOnce(a.ctx)
	require.NoError(t, err)
	require.Equal(t, "70.00 BRL", a.saldo(t, w))

	a.publicar(t, "d2", mensagemDeAposta("msg-1", "tx-outra", w, p, "BET", "10.00"))
	_, err = a.consumer.RunOnce(a.ctx)

	require.NoError(t, err)
	assert.Equal(t, "70.00 BRL", a.saldo(t, w))

	// Uma leitura só: consultar a fila duas vezes seguidas devolve resultados
	// diferentes, porque a primeira já tirou a mensagem de vista.
	descartadas := a.naDLQ(t)
	require.Len(t, descartadas, 1)
	assert.Contains(t, descartadas[0], "divergente")
}

// AC-3 pelo transporte: a mesma mensagem entregue duas vezes.
func TestReentregaIdenticaNaoDuplicaODebito(t *testing.T) {
	a := novoAmbienteConsumo(t)
	w, p := a.carteira(t, "100.00")
	corpo := mensagemDeAposta("msg-1", "tx-1", w, p, "BET", "30.00")

	a.publicar(t, "d1", corpo)
	_, err := a.consumer.RunOnce(a.ctx)
	require.NoError(t, err)

	a.publicar(t, "d2", corpo)
	tratadas, err := a.consumer.RunOnce(a.ctx)

	require.NoError(t, err)
	assert.Equal(t, 1, tratadas, "a reentrega é tratada, mas não reprocessada")
	assert.Equal(t, "70.00 BRL", a.saldo(t, w), "um único débito")
	assert.Equal(t, int64(2), a.conta(t, `SELECT count(*) FROM wallet_ledger_entries
		WHERE wallet_id = ?`, uuid.UUID(w)))
	assert.Zero(t, a.restamNaFila(t))
	assert.Empty(t, a.naDLQ(t))
}

// AC-E1
func TestFilaVaziaTerminaSemErro(t *testing.T) {
	a := novoAmbienteConsumo(t)

	tratadas, err := a.consumer.RunOnce(a.ctx)

	require.NoError(t, err)
	assert.Zero(t, tratadas)
}

// A recusa de negócio é terminal também pelo transporte: sai da fila e não
// volta, com o desfecho publicado.
func TestRecusaDeNegocioSaiDaFilaComEventoPublicado(t *testing.T) {
	a := novoAmbienteConsumo(t)
	w, p := a.carteira(t, "10.00")
	a.publicar(t, "d1", mensagemDeAposta("msg-1", "tx-1", w, p, "BET", "500.00"))

	_, err := a.consumer.RunOnce(a.ctx)

	require.NoError(t, err)
	assert.Zero(t, a.restamNaFila(t))
	assert.Empty(t, a.naDLQ(t), "recusa de negócio não é mensagem inválida")
	assert.Equal(t, int64(1), a.conta(t, `SELECT count(*) FROM wager_transactions
		WHERE external_transaction_id = 'tx-1' AND status = 'REJECTED'
		  AND failure_code = 'INSUFFICIENT_FUNDS'`))
	assert.Equal(t, int64(1), a.conta(t, `SELECT count(*) FROM outbox_events
		WHERE aggregate_id = ? AND event_type = 'WagerTransactionRejected'`, uuid.UUID(w)))
}

// TestEncerramentoDevolveAFilaOQueNaoVaiTratar guarda o comportamento que o §10
// exige no SIGTERM.
//
// O enunciado dá duas saídas: concluir o trabalho em andamento dentro do prazo,
// **ou** liberar a visibilidade para reentrega segura. A escolha é a segunda —
// concluir manteria viva uma transação financeira enquanto o processo morre, e
// um encerramento que espera pelo banco é um encerramento que pode não
// acontecer.
//
// Antes desta devolução o sistema não fazia nenhuma das duas. Era seguro (a
// transação desfaz, a mensagem não sai da fila, a reentrega acontece), mas só
// depois de o visibility timeout expirar: trinta segundos de atraso por mensagem
// em voo, a cada reinício. O `ARCHITECTURE.md` afirmava que liberava a
// visibilidade, e não liberava — foi assim que a auditoria contra o enunciado
// encontrou isto.
//
// O teste força a fila a ter visibilidade LONGA. Sem a devolução, o que sobrou
// do lote ficaria invisível bem além da duração do caso, e o assert final
// falharia.
func TestEncerramentoDevolveAFilaOQueNaoVaiTratar(t *testing.T) {
	a := novoAmbienteConsumo(t)
	_, err := adaptersqs.API(a.cliente).SetQueueAttributes(a.ctx, &awssqs.SetQueueAttributesInput{
		QueueUrl:   aws.String(a.cfg.TransactionsQueueURL),
		Attributes: map[string]string{"VisibilityTimeout": "120"},
	})
	require.NoError(t, err)

	w, p := a.carteira(t, "1000.00")

	// O cenário só vale se o consumidor tratou ao menos uma e sobrou ao menos
	// uma. Em vez de torcer pelo relógio, o caso repete com folga crescente até
	// o cenário acontecer — e falha se nunca acontecer.
	var tratadas int
	for _, folga := range []time.Duration{20, 60, 150, 400, 1000} {
		sufixo := uuid.New().String()[:8]
		for i := 0; i < 6; i++ {
			id := fmt.Sprintf("m-%s-%d", sufixo, i)
			a.publicar(t, id, mensagemDeAposta(id, id, w, p, "BET", "1.00"))
		}

		ctx, cancelar := context.WithCancel(a.ctx)
		go func() { time.Sleep(folga * time.Millisecond); cancelar() }()
		tratadas, _ = a.consumer.RunOnce(ctx)
		cancelar()

		if tratadas >= 1 && tratadas < 6 {
			break
		}
		// Cenário inconclusivo: drena o que sobrou e tenta com mais folga.
		for a.restamNaFila(t) > 0 {
			ctxLimpo, cancelarLimpo := context.WithCancel(a.ctx)
			_, _ = a.consumer.RunOnce(ctxLimpo)
			cancelarLimpo()
		}
		tratadas = 0
	}
	require.NotZero(t, tratadas,
		"não consegui interromper o lote no meio; o caso não teria o que provar")

	// O essencial: o que sobrou está visível AGORA, e não daqui a dois minutos.
	visiveis := a.restamNaFila(t)
	assert.Equal(t, 6-tratadas, visiveis,
		"o lote interrompido tem de voltar inteiro à fila, na hora")

	// E devolver não é reprocessar: o que já tinha sido tratado não voltou.
	assert.Equal(t, int64(tratadas), a.conta(t,
		`SELECT count(*) FROM wager_transactions WHERE kind = 'BET' AND status = 'PROCESSED'`))
}
