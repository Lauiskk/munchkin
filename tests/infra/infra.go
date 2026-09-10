// Package infra sobe a infraestrutura real de que uma suíte precisa.
//
// Vive sob tests/ e importa `testing` de propósito: é código de teste, e fingir
// o contrário — devolvendo erros para o chamador tratar — só acrescentaria
// cerimônia a algo que sempre termina em `t.Fatal`.
package infra

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tclocalstack "github.com/testcontainers/testcontainers-go/modules/localstack"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/Lauiskk/munchkin/internal/adapter/postgres"
	adaptersqs "github.com/Lauiskk/munchkin/internal/adapter/sqs"
	"github.com/Lauiskk/munchkin/internal/config"
)

const (
	Realm    = "munchkin"
	Audience = "munchkin-api"

	postgresImage   = "postgres:18.6-alpine"
	localstackImage = "localstack/localstack:4.9"
	keycloakImage   = "quay.io/keycloak/keycloak:26.7.3"

	dbName        = "munchkin"
	ownerUser     = "munchkin_owner"
	ownerPassword = "teste-owner"
	appUser       = "munchkin_app"
	appPassword   = "teste-app"
)

// Stack é a infraestrutura de uma suíte.
type Stack struct {
	DB          config.DB
	AWS         config.AWS
	KeycloakURL string
}

// Env devolve as variáveis que a aplicação precisa para falar com esta pilha.
func (s Stack) Env() map[string]string {
	return map[string]string{
		"APP_ENV":                    "test",
		"LOG_LEVEL":                  "info",
		"LOG_FORMAT":                 "json",
		"AUTH_ISSUER":                s.KeycloakURL + "/realms/" + Realm,
		"AUTH_DISCOVERY_URL":         s.KeycloakURL + "/realms/" + Realm + "/.well-known/openid-configuration",
		"AUTH_AUDIENCE":              Audience,
		"DB_HOST":                    s.DB.Host,
		"DB_PORT":                    fmt.Sprint(s.DB.Port),
		"DB_NAME":                    s.DB.Name,
		"DB_USER":                    s.DB.User,
		"DB_PASSWORD":                s.DB.Password.Reveal(),
		"DB_SSLMODE":                 "disable",
		"AWS_REGION":                 s.AWS.Region,
		"AWS_ENDPOINT_URL":           s.AWS.Endpoint,
		"AWS_ACCESS_KEY_ID":          s.AWS.AccessKeyID.Reveal(),
		"AWS_SECRET_ACCESS_KEY":      s.AWS.SecretAccessKey.Reveal(),
		"AWS_EVENTS_QUEUE_URL":       s.AWS.EventsQueueURL,
		"AWS_TRANSACTIONS_QUEUE_URL": s.AWS.TransactionsQueueURL,
		"AWS_TRANSACTIONS_DLQ_URL":   s.AWS.TransactionsDLQURL,
		// Intervalos curtos: a suíte espera por CONDIÇÃO, e um intervalo longo
		// só faria cada espera demorar mais para chegar à mesma conclusão.
		"WORKER_REFERENCE_INTERVAL": "1s",
		"WORKER_OUTBOX_INTERVAL":    "1s",
		"WORKER_OUTBOX_LEASE":       "5s",
		"WORKER_CONSUMER_INTERVAL":  "1s",
		"WORKER_CONSUMER_WAIT":      "1s",
	}
}

var (
	umaVez     sync.Once
	pilha      Stack
	pilhaErr   error
	encerrarem []func()
)

// Start devolve a pilha da suíte, subindo-a na primeira chamada.
//
// Compartilhada entre os testes do pacote porque o custo é alto — o Keycloak
// sozinho leva dezenas de segundos — e porque não há o que isolar: cada cenário
// usa carteiras próprias, e os PROCESSOS, que são o objeto sob prova aqui, esses
// sim sobem e morrem por teste.
//
// O encerramento é do TestMain: t.Cleanup derrubaria a pilha ao fim do primeiro
// teste, e o segundo encontraria containers mortos.
func Start(t *testing.T) Stack {
	t.Helper()
	umaVez.Do(func() {
		defer func() {
			if r := recover(); r != nil {
				pilhaErr = fmt.Errorf("falha ao subir a infraestrutura: %v", r)
			}
		}()
		pilha = Stack{DB: startPostgres(t)}
		pilha.AWS = startLocalStack(t)
		pilha.KeycloakURL = startKeycloak(t)
		aplicarMigrations(t, pilha.DB)
	})
	require.NoError(t, pilhaErr)
	return pilha
}

// Stop derruba a infraestrutura. Chamado pelo TestMain da suíte.
func Stop() {
	for _, f := range encerrarem {
		f()
	}
	encerrarem = nil
}

func startPostgres(t *testing.T) config.DB {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	script, err := filepath.Abs(filepath.Join("..", "..", "deploy", "postgres", "10-roles.sh"))
	require.NoError(t, err)

	c, err := tcpostgres.Run(ctx, postgresImage,
		tcpostgres.WithDatabase(dbName),
		tcpostgres.WithUsername(ownerUser),
		tcpostgres.WithPassword(ownerPassword),
		tcpostgres.WithInitScripts(script),
		testcontainers.WithEnv(map[string]string{
			"MUNCHKIN_APP_USER":     appUser,
			"MUNCHKIN_APP_PASSWORD": appPassword,
		}),
		testcontainers.WithWaitStrategy(wait.ForAll(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
			wait.ForListeningPort("5432/tcp"),
		).WithStartupTimeoutDefault(2*time.Minute)),
	)
	require.NoError(t, err, "Docker precisa estar em execução")
	encerrar(t, c)

	host, porta := endereco(t, c, "5432/tcp")
	return config.DB{
		Host: host, Port: porta, Name: dbName,
		User: appUser, Password: config.Secret(appPassword),
		SSLMode: "disable", MaxOpenConns: 20, MaxIdleConns: 5,
		ConnMaxLifetime: 30 * time.Minute, ConnMaxIdleTime: 5 * time.Minute,
		ConnectTimeout: 10 * time.Second,
	}
}

func startLocalStack(t *testing.T) config.AWS {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	c, err := tclocalstack.Run(ctx, localstackImage,
		testcontainers.WithEnv(map[string]string{"SERVICES": "sqs"}),
		testcontainers.WithWaitStrategy(wait.ForAll(
			wait.ForLog("Ready."),
			wait.ForListeningPort("4566/tcp"),
		).WithStartupTimeoutDefault(2*time.Minute)),
	)
	require.NoError(t, err, "Docker precisa estar em execução")
	encerrar(t, c)

	host, porta := endereco(t, c, "4566/tcp")
	cfg := config.AWS{
		Region:          "us-east-1",
		Endpoint:        fmt.Sprintf("http://%s:%d", host, porta),
		AccessKeyID:     config.Secret("test"),
		SecretAccessKey: config.Secret("test"),
	}

	cliente, err := adaptersqs.New(config.Config{AWS: cfg})
	require.NoError(t, err)

	criar := func(nome string) string {
		saida, err := adaptersqs.API(cliente).CreateQueue(ctx, &awssqs.CreateQueueInput{
			QueueName: aws.String(nome),
			Attributes: map[string]string{
				"FifoQueue": "true", "ContentBasedDeduplication": "false",
				"VisibilityTimeout": "5",
			},
		})
		require.NoError(t, err)
		return *saida.QueueUrl
	}
	sufixo := strings.ToLower(uuid.New().String()[:8])
	cfg.EventsQueueURL = criar("eventos-" + sufixo + ".fifo")
	cfg.TransactionsQueueURL = criar("ops-" + sufixo + ".fifo")
	cfg.TransactionsDLQURL = criar("dlq-" + sufixo + ".fifo")
	return cfg
}

func startKeycloak(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	realmPath, err := filepath.Abs(filepath.Join("..", "..", "deploy", "keycloak", "realm-munchkin.json"))
	require.NoError(t, err)

	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		Started: true,
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        keycloakImage,
			Cmd:          []string{"start-dev", "--import-realm"},
			ExposedPorts: []string{"8080/tcp"},
			Env: map[string]string{
				"KC_BOOTSTRAP_ADMIN_USERNAME": "admin",
				"KC_BOOTSTRAP_ADMIN_PASSWORD": "admin",
				"KC_HTTP_PORT":                "8080",
				"KC_HEALTH_ENABLED":           "true",
				"KC_HOSTNAME_STRICT":          "false",
			},
			// Montado, e não copiado: o diretório de import não existe na
			// imagem, e copiar falha em silêncio — o realm nunca é importado.
			HostConfigModifier: func(hc *container.HostConfig) {
				hc.Binds = append(hc.Binds,
					realmPath+":/opt/keycloak/data/import/realm-munchkin.json:ro")
			},
			WaitingFor: wait.ForListeningPort("8080/tcp").WithStartupTimeout(3 * time.Minute),
		},
	})
	require.NoError(t, err, "Docker precisa estar em execução")
	encerrar(t, c)

	host, porta := endereco(t, c, "8080/tcp")
	base := fmt.Sprintf("http://%s:%d", host, porta)
	esperarRealm(t, ctx, base)
	return base
}

// esperarRealm sonda a descoberta: a porta abre antes de o realm existir.
func esperarRealm(t *testing.T, ctx context.Context, base string) {
	t.Helper()
	alvo := base + "/realms/" + Realm + "/.well-known/openid-configuration"
	prazo := time.Now().Add(3 * time.Minute)

	for time.Now().Before(prazo) {
		resp, err := buscar(ctx, alvo)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("o realm %q não ficou disponível em %s", Realm, base)
}

func aplicarMigrations(t *testing.T, db config.DB) {
	t.Helper()
	dono := db
	dono.User = ownerUser
	dono.Password = config.Secret(ownerPassword)

	m, err := postgres.NewMigrator(dono, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	require.NoError(t, m.Up())
	require.NoError(t, m.Close())
}

// Conectar abre um pool para as asserções do teste.
func (s Stack) Conectar(t *testing.T) *postgres.Database {
	t.Helper()
	db, err := postgres.Open(
		config.Config{DB: s.DB, Log: config.Log{Level: "info"}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func endereco(t *testing.T, c testcontainers.Container, porta string) (string, int) {
	t.Helper()
	ctx := context.Background()
	host, err := c.Host(ctx)
	require.NoError(t, err)
	p, err := c.MappedPort(ctx, porta)
	require.NoError(t, err)
	return host, int(p.Num())
}

func encerrar(_ *testing.T, c testcontainers.Container) {
	encerrarem = append(encerrarem, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = c.Terminate(ctx)
	})
}

// buscar faz um GET simples, respeitando o contexto.
func buscar(ctx context.Context, alvo string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, alvo, nil)
	if err != nil {
		return nil, err
	}
	return (&http.Client{Timeout: 5 * time.Second}).Do(req)
}

// Publicar coloca uma mensagem na fila de entrada de operações.
func (s Stack) Publicar(t *testing.T, dedup, corpo string) {
	t.Helper()
	cliente, err := adaptersqs.New(config.Config{AWS: s.AWS})
	require.NoError(t, err)

	_, err = adaptersqs.API(cliente).SendMessage(context.Background(), &awssqs.SendMessageInput{
		QueueUrl:               aws.String(s.AWS.TransactionsQueueURL),
		MessageBody:            aws.String(corpo),
		MessageGroupId:         aws.String("recuperacao"),
		MessageDeduplicationId: aws.String(dedup),
	})
	require.NoError(t, err)
}

// MensagensNaFila devolve quantas mensagens estão visíveis ou em voo.
func (s Stack) MensagensNaFila(t *testing.T) int64 {
	t.Helper()
	cliente, err := adaptersqs.New(config.Config{AWS: s.AWS})
	require.NoError(t, err)

	saida, err := adaptersqs.API(cliente).GetQueueAttributes(context.Background(),
		&awssqs.GetQueueAttributesInput{
			QueueUrl: aws.String(s.AWS.TransactionsQueueURL),
			AttributeNames: []types.QueueAttributeName{
				types.QueueAttributeNameApproximateNumberOfMessages,
				types.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
			},
		})
	require.NoError(t, err)

	total := int64(0)
	for _, k := range []types.QueueAttributeName{
		types.QueueAttributeNameApproximateNumberOfMessages,
		types.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
	} {
		var n int64
		_, _ = fmt.Sscan(saida.Attributes[string(k)], &n)
		total += n
	}
	return total
}
