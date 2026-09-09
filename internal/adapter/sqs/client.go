// Package sqs adapta o SQS ao que a aplicação precisa dele.
package sqs

import (
	"context"
	"fmt"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/Lauiskk/munchkin/internal/config"
)

// Client embrulha o cliente do SDK com a configuração já resolvida.
type Client struct {
	api *awssqs.Client
	cfg config.AWS
}

// New monta o cliente.
//
// As credenciais são estáticas e vêm da configuração validada no boot, em vez
// da cadeia padrão do SDK. A cadeia padrão procura em variáveis de ambiente,
// arquivo de perfil e metadata do host — o que torna "de onde veio esta
// credencial" uma pergunta sem resposta determinística, e faz o mesmo binário
// se comportar diferente conforme a máquina.
func New(cfg config.Config) (*Client, error) {
	carregada, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(cfg.AWS.Region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			cfg.AWS.AccessKeyID.Reveal(), cfg.AWS.SecretAccessKey.Reveal(), "")),
	)
	if err != nil {
		return nil, fmt.Errorf("configuração da AWS: %w", err)
	}

	opcoes := []func(*awssqs.Options){}
	if cfg.AWS.Endpoint != "" {
		// Endpoint explícito é o caminho do LocalStack. Vazio em produção, onde
		// o SDK resolve o endereço real a partir da região.
		endpoint := cfg.AWS.Endpoint
		opcoes = append(opcoes, func(o *awssqs.Options) { o.BaseEndpoint = &endpoint })
	}

	return &Client{api: awssqs.NewFromConfig(carregada, opcoes...), cfg: cfg.AWS}, nil
}

// API expõe o cliente do SDK.
//
// Existe para os testes de integração, que precisam falar com a fila
// diretamente para conferir o que chegou. O código da aplicação usa o
// Publisher: nenhum caso de uso alcança este ponteiro.
func API(c *Client) *awssqs.Client { return c.api }
