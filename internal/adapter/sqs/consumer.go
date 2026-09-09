package sqs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/Lauiskk/munchkin/internal/adapter/contract"
	"github.com/Lauiskk/munchkin/internal/app/inbox"
	appwagering "github.com/Lauiskk/munchkin/internal/app/wagering"
	"github.com/Lauiskk/munchkin/pkg/correlation"
	"github.com/Lauiskk/munchkin/pkg/logs"
)

// maxMotivo limita o texto que acompanha uma mensagem enviada à DLQ.
const maxMotivo = 900

// Consumer lê operações da fila e as entrega ao tratamento.
type Consumer struct {
	client   *Client
	handler  *inbox.Handler
	log      *slog.Logger
	lote     int32
	esperaMS int32
}

// NewConsumer monta o consumidor.
func NewConsumer(c *Client, h *inbox.Handler, log *slog.Logger, lote int, espera time.Duration) *Consumer {
	// O SQS limita o lote a 10 e a espera a 20s. Passar disso é erro de
	// requisição, não uma configuração generosa.
	if lote > 10 {
		lote = 10
	}
	if espera > 20*time.Second {
		espera = 20 * time.Second
	}
	return &Consumer{
		client: c, handler: h, log: log,
		lote: int32(lote), esperaMS: int32(espera.Seconds()),
	}
}

// RunOnce busca um lote e trata cada mensagem. Devolve quantas foram tratadas.
func (c *Consumer) RunOnce(ctx context.Context) (int, error) {
	recebidas, err := c.client.api.ReceiveMessage(ctx, &awssqs.ReceiveMessageInput{
		QueueUrl:            aws.String(c.client.cfg.TransactionsQueueURL),
		MaxNumberOfMessages: c.lote,
		WaitTimeSeconds:     c.esperaMS,
	})
	if err != nil {
		// O erro sobe mesmo durante o encerramento. Quem decide se vale
		// registrar é o worker, que já silencia falha com contexto cancelado —
		// engolir aqui esconderia também as falhas que não são encerramento.
		return 0, fmt.Errorf("leitura da fila de operações: %w", err)
	}

	tratadas := 0
	for _, m := range recebidas.Messages {
		if ctx.Err() != nil {
			// Encerramento: o que não foi tratado volta a ficar visível quando
			// o visibility timeout expirar, e outra instância pega.
			return tratadas, ctx.Err()
		}
		if c.tratar(ctx, m) {
			tratadas++
		}
	}
	return tratadas, nil
}

// tratar processa uma mensagem e decide o destino dela.
//
// Devolve se a mensagem saiu da fila. Não devolve erro: cada mensagem é
// independente, e uma que falha não pode impedir as seguintes de serem tratadas.
func (c *Consumer) tratar(ctx context.Context, m types.Message) bool {
	corpo := aws.ToString(m.Body)

	// Correlação própria por mensagem: sem isso, tudo que o tratamento registra
	// ficaria sem como ser amarrado à mensagem que o originou.
	ctx = correlation.Into(ctx, correlation.New())

	decodificada, err := contract.DecodeMessage([]byte(corpo))
	if err != nil {
		// Erro permanente. Retentar não muda o resultado: o conteúdo é o mesmo,
		// e a validação também. Gastar as cinco entregas para chegar à mesma
		// conclusão só atrasa o diagnóstico.
		c.descartar(ctx, m, "mensagem inválida: "+err.Error())
		return false
	}

	registro := c.log.With(
		slog.String(logs.KeyMessageID, decodificada.MessageID),
		slog.String(logs.KeyProviderID, string(decodificada.Transaction.ProviderID)),
		slog.String(logs.KeyCorrelationID, correlation.From(ctx)))

	desfecho, saida, err := c.handler.Handle(ctx, inbox.Message{
		ID:    decodificada.MessageID,
		Hash:  contract.HashMessage([]byte(corpo)),
		Input: entrada(decodificada),
	})
	if err != nil {
		// Falha transitória: a mensagem NÃO é removida. Ela volta a ficar
		// visível e é retentada; esgotado o maxReceiveCount, o redrive a leva
		// à DLQ sem que a aplicação precise decidir nada.
		registro.LogAttrs(ctx, slog.LevelError, "consumer.failed",
			slog.String(logs.KeyError, err.Error()))
		return false
	}

	if desfecho == inbox.HashMismatch {
		// Mesma identidade durável, conteúdo diferente. Aceitar seria deixar
		// alguém reescrever o passado reusando um identificador.
		c.descartar(ctx, m, "reentrega com conteúdo divergente para o mesmo messageId")
		return false
	}

	// Processada ou duplicada, a mensagem sai da fila. Recusa de negócio também
	// é desfecho: ela está persistida, com código de falha e evento publicado,
	// e retentar produziria a mesma recusa para sempre.
	c.remover(ctx, m)
	registro.LogAttrs(ctx, slog.LevelInfo, "consumer.handled",
		slog.String("outcome", string(desfecho)),
		slog.String("status", string(saida.Status)),
		slog.String("failureCode", string(saida.FailureCode)))
	return true
}

// entrada converte a mensagem decodificada na entrada do caso de uso.
func entrada(m contract.DecodedMessage) appwagering.Input {
	t := m.Transaction
	return appwagering.Input{
		ProviderID:          t.ProviderID,
		ExternalID:          t.ExternalID,
		IdempotencyKey:      m.IdempotencyKey,
		PlayerID:            t.PlayerID,
		WalletID:            t.WalletID,
		RoundID:             t.RoundID,
		GameID:              t.GameID,
		Kind:                t.Kind,
		Money:               t.Money,
		ReferenceExternalID: t.ReferenceExternalID,
	}
}

// descartar manda a mensagem para a DLQ e a remove da fila de origem.
//
// O redrive automático levaria a mesma mensagem à DLQ depois de cinco
// entregas. Para erro permanente isso é só demora: o resultado é conhecido na
// primeira tentativa, e quatro reentregas não acrescentam informação.
func (c *Consumer) descartar(ctx context.Context, m types.Message, motivo string) {
	id := aws.ToString(m.MessageId)
	c.log.LogAttrs(ctx, slog.LevelWarn, "consumer.dead_lettered",
		slog.String(logs.KeyMessageID, id),
		slog.String(logs.KeyCorrelationID, correlation.From(ctx)),
		slog.String("reason", truncar(motivo)))

	_, err := c.client.api.SendMessage(ctx, &awssqs.SendMessageInput{
		QueueUrl:    aws.String(c.client.cfg.TransactionsDLQURL),
		MessageBody: m.Body,
		// O corpo original vai inteiro: sem ele a DLQ é um contador, não uma
		// ferramenta de diagnóstico. É a mesma informação que já estava na fila
		// de origem.
		MessageGroupId:         aws.String(id),
		MessageDeduplicationId: aws.String(id),
		MessageAttributes: map[string]types.MessageAttributeValue{
			"reason": {DataType: aws.String("String"), StringValue: aws.String(truncar(motivo))},
		},
	})
	if err != nil {
		// Sem conseguir mover, a mensagem fica onde está. O redrive da fila a
		// levará à DLQ depois das entregas configuradas — mais devagar, mas
		// sem perder nada.
		c.log.LogAttrs(ctx, slog.LevelError, "consumer.dead_letter_failed",
			slog.String(logs.KeyMessageID, id),
			slog.String(logs.KeyError, err.Error()))
		return
	}
	c.remover(ctx, m)
}

// remover apaga a mensagem da fila de origem.
func (c *Consumer) remover(ctx context.Context, m types.Message) {
	_, err := c.client.api.DeleteMessage(ctx, &awssqs.DeleteMessageInput{
		QueueUrl:      aws.String(c.client.cfg.TransactionsQueueURL),
		ReceiptHandle: m.ReceiptHandle,
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		// A remoção falhou DEPOIS do commit. A mensagem será reentregue, e a
		// inbox reconhecerá o tratamento já concluído sem reprocessar — que é
		// exatamente o cenário para o qual ela existe.
		c.log.LogAttrs(ctx, slog.LevelWarn, "consumer.delete_failed",
			slog.String(logs.KeyMessageID, aws.ToString(m.MessageId)),
			slog.String(logs.KeyError, err.Error()))
	}
}

func truncar(s string) string {
	if len(s) <= maxMotivo {
		return s
	}
	return s[:maxMotivo]
}
