package sqs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"go.uber.org/fx"

	"github.com/Lauiskk/munchkin/internal/adapter/http/handler"
	"github.com/Lauiskk/munchkin/internal/app/outbox"
)

// probeTimeout limita a sonda de prontidão da fila.
const probeTimeout = 2 * time.Second

// readinessCheck responde pela prontidão do SQS.
//
// Pergunta pelos atributos da fila de eventos, não apenas se o serviço
// responde. Uma fila que não existe torna o publicador inútil, e o §9 do
// enunciado pede prontidão do SQS — não do endpoint.
type readinessCheck struct{ client *Client }

func (c readinessCheck) Name() string { return "sqs" }

func (c readinessCheck) Check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	_, err := c.client.api.GetQueueAttributes(ctx, &awssqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(c.client.cfg.EventsQueueURL),
		AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn},
	})
	if err != nil {
		return fmt.Errorf("fila de eventos indisponível: %w", err)
	}
	return nil
}

func newReadinessCheck(c *Client) handler.ReadinessCheck { return readinessCheck{client: c} }

// register verifica o acesso à fila na subida.
//
// Ao contrário do banco, uma fila inalcançável NÃO impede a aplicação de subir.
// A publicação é assíncrona por desenho: os eventos continuam sendo gravados na
// transação, e o worker os publica quando o transporte voltar. Recusar a subida
// aqui tiraria do ar o caminho financeiro inteiro por causa de uma dependência
// que ele não usa.
func register(lc fx.Lifecycle, c *Client, log *slog.Logger) {
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			if err := (readinessCheck{client: c}).Check(ctx); err != nil {
				log.Warn("sqs.unreachable_at_start",
					slog.String("queue", c.cfg.EventsQueueURL),
					slog.String("error", err.Error()))
				return nil
			}
			log.Info("sqs.connected",
				slog.String("region", c.cfg.Region),
				slog.String("queue", c.cfg.EventsQueueURL))
			return nil
		},
	})
}

// Module provê o acesso ao SQS.
var Module = fx.Module("sqs",
	fx.Provide(
		New,
		NewPublisher,
		fx.Annotate(newReadinessCheck, fx.ResultTags(`group:"readiness"`)),
		func(p *Publisher) outbox.Publisher { return p },
	),
	fx.Invoke(register),
)
