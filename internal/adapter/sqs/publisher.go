package sqs

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/Lauiskk/munchkin/internal/app/outbox"
)

// Publisher entrega eventos na fila de saída.
type Publisher struct {
	client *Client
}

// NewPublisher monta o publicador sobre a fila de eventos configurada.
func NewPublisher(c *Client) *Publisher { return &Publisher{client: c} }

// Publish envia uma mensagem para a fila de eventos.
//
// O identificador de deduplicação é o eventId, e o de grupo é a carteira. Os
// dois vêm prontos do caso de uso: este adaptador não decide contrato de
// roteamento, apenas o executa.
func (p *Publisher) Publish(ctx context.Context, m outbox.Message) error {
	_, err := p.client.api.SendMessage(ctx, &awssqs.SendMessageInput{
		QueueUrl:               aws.String(p.client.cfg.EventsQueueURL),
		MessageBody:            aws.String(string(m.Body)),
		MessageGroupId:         aws.String(m.GroupID),
		MessageDeduplicationId: aws.String(m.DeduplicationID),
	})
	if err != nil {
		// A mensagem do SDK entra no erro, mas o corpo do evento não: quem lê
		// o log precisa saber que falhou e por quê, não o conteúdo financeiro.
		return fmt.Errorf("envio para a fila de eventos: %w", err)
	}
	return nil
}
