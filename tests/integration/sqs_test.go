//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tclocalstack "github.com/testcontainers/testcontainers-go/modules/localstack"
	"github.com/testcontainers/testcontainers-go/wait"

	adaptersqs "github.com/Lauiskk/munchkin/internal/adapter/sqs"
	"github.com/Lauiskk/munchkin/internal/app/outbox"
	"github.com/Lauiskk/munchkin/internal/config"
)

const localstackImage = "localstack/localstack:4.9"

// localstackCompartilhado é um único container para toda a suíte.
//
// Um por teste era o desenho anterior, e ele derrubava a suíte: somados aos
// PostgreSQL por teste, os containers do LocalStack sobrecarregavam a máquina e
// testes sem relação nenhuma começavam a estourar prazo. O isolamento que
// importa aqui não é o container — é a fila, e essa continua sendo uma por
// teste.
var (
	localstackUmaVez sync.Once
	localstackCfg    config.AWS
	localstackErr    error
	localstackParar  func()
)

func TestMain(m *testing.M) {
	codigo := m.Run()
	if localstackParar != nil {
		localstackParar()
	}
	os.Exit(codigo)
}

func localstackCompartilhado() (config.AWS, error) {
	localstackUmaVez.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()

		container, err := tclocalstack.Run(ctx, localstackImage,
			testcontainers.WithEnv(map[string]string{"SERVICES": "sqs"}),
			testcontainers.WithWaitStrategy(
				wait.ForAll(
					wait.ForLog("Ready."),
					wait.ForListeningPort("4566/tcp"),
				).WithStartupTimeoutDefault(2*time.Minute)),
		)
		if err != nil {
			localstackErr = err
			return
		}
		localstackParar = func() {
			c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = container.Terminate(c)
		}

		host, err := container.Host(ctx)
		if err != nil {
			localstackErr = err
			return
		}
		porta, err := container.MappedPort(ctx, "4566/tcp")
		if err != nil {
			localstackErr = err
			return
		}

		localstackCfg = config.AWS{
			Region:          "us-east-1",
			Endpoint:        "http://" + host + ":" + porta.Port(),
			AccessKeyID:     config.Secret("test"),
			SecretAccessKey: config.Secret("test"),
		}
	})
	return localstackCfg, localstackErr
}

// filaDeTeste cria uma fila FIFO exclusiva deste teste e devolve a
// configuração que aponta para ela.
//
// SQS de verdade, não dublê: o enunciado trata como eliminatória a substituição
// integral de PostgreSQL, SQS e IdP por mocks. Um dublê aqui esconderia
// justamente o que só o serviço real cobra — atributos obrigatórios de fila
// FIFO, formato do identificador de deduplicação e limites do corpo.
func filaDeTeste(t *testing.T) config.AWS {
	t.Helper()

	cfg, err := localstackCompartilhado()
	require.NoError(t, err, "Docker precisa estar em execução para os testes de integração")

	cliente, err := adaptersqs.New(config.Config{AWS: cfg})
	require.NoError(t, err)

	// Nome por teste: o container é compartilhado, a fila não. Sem isso, uma
	// mensagem deixada por um teste apareceria como resultado de outro.
	nome := "eventos-" + strings.ToLower(uuid.New().String()) + ".fifo"

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	saida, err := adaptersqs.API(cliente).CreateQueue(ctx, &awssqs.CreateQueueInput{
		QueueName: aws.String(nome),
		Attributes: map[string]string{
			"FifoQueue":                 "true",
			"ContentBasedDeduplication": "false",
		},
	})
	require.NoError(t, err)

	cfg.EventsQueueURL = *saida.QueueUrl
	return cfg
}

// AC-1, AC-8, AC-9 — a publicação real, contra um SQS real.
func TestPublicacaoChegaNaFilaComOsAtributosDoContrato(t *testing.T) {
	cfg := filaDeTeste(t)
	ctx := context.Background()

	cliente, err := adaptersqs.New(config.Config{AWS: cfg})
	require.NoError(t, err)
	publicador := adaptersqs.NewPublisher(cliente)

	corpo := []byte(`{"eventId":"01a086a2-8447-723c-80bf-f3dba598cc4f","eventType":"WagerTransactionProcessed"}`)
	require.NoError(t, publicador.Publish(ctx, outbox.Message{
		DeduplicationID: "01a086a2-8447-723c-80bf-f3dba598cc4f",
		GroupID:         "01a086a2-8439-7cef-939e-43373b0f3764",
		Body:            corpo,
	}))

	recebidas, err := adaptersqs.API(cliente).ReceiveMessage(ctx, &awssqs.ReceiveMessageInput{
		QueueUrl:                    aws.String(cfg.EventsQueueURL),
		MaxNumberOfMessages:         10,
		WaitTimeSeconds:             5,
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameAll},
	})
	require.NoError(t, err)
	require.Len(t, recebidas.Messages, 1)

	m := recebidas.Messages[0]
	assert.JSONEq(t, string(corpo), *m.Body)
	assert.Equal(t, "01a086a2-8447-723c-80bf-f3dba598cc4f",
		m.Attributes[string(types.MessageSystemAttributeNameMessageDeduplicationId)],
		"o identificador de deduplicação é o eventId: é ele que torna a republicação inócua")
	assert.Equal(t, "01a086a2-8439-7cef-939e-43373b0f3764",
		m.Attributes[string(types.MessageSystemAttributeNameMessageGroupId)],
		"o grupo é a carteira: ordena os eventos dela sem serializar carteiras distintas")
}

// A deduplicação da fila é a segunda linha de defesa da republicação. Se ela
// não valesse, uma queda entre publicar e confirmar entregaria o mesmo evento
// duas vezes ao consumidor.
func TestRepublicacaoComOMesmoEventIDNaoDuplicaNaFila(t *testing.T) {
	cfg := filaDeTeste(t)
	ctx := context.Background()

	cliente, err := adaptersqs.New(config.Config{AWS: cfg})
	require.NoError(t, err)
	publicador := adaptersqs.NewPublisher(cliente)

	msg := outbox.Message{
		DeduplicationID: "01a086a2-8447-723c-80bf-f3dba598cc4f",
		GroupID:         "01a086a2-8439-7cef-939e-43373b0f3764",
		Body:            []byte(`{"eventId":"01a086a2-8447-723c-80bf-f3dba598cc4f"}`),
	}
	require.NoError(t, publicador.Publish(ctx, msg))
	require.NoError(t, publicador.Publish(ctx, msg))

	var total int
	for range 2 {
		recebidas, err := adaptersqs.API(cliente).ReceiveMessage(ctx, &awssqs.ReceiveMessageInput{
			QueueUrl:            aws.String(cfg.EventsQueueURL),
			MaxNumberOfMessages: 10,
			WaitTimeSeconds:     3,
			VisibilityTimeout:   0,
		})
		require.NoError(t, err)
		total = max(total, len(recebidas.Messages))
	}
	assert.Equal(t, 1, total, "a fila FIFO descarta a segunda entrega do mesmo eventId")
}

// Um corpo que não é JSON válido não é problema do transporte — o SQS aceita
// qualquer texto. O teste existe para deixar explícito de quem é a
// responsabilidade: a forma do envelope é garantida antes daqui.
func TestTransporteNaoValidaOConteudo(t *testing.T) {
	cfg := filaDeTeste(t)
	cliente, err := adaptersqs.New(config.Config{AWS: cfg})
	require.NoError(t, err)

	err = adaptersqs.NewPublisher(cliente).Publish(context.Background(), outbox.Message{
		DeduplicationID: "01a086a2-8447-723c-80bf-f3dba598cc4f",
		GroupID:         "g1",
		Body:            []byte("nem json é"),
	})
	assert.NoError(t, err)

	var envelope map[string]any
	assert.Error(t, json.Unmarshal([]byte("nem json é"), &envelope))
}
