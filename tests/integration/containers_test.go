//go:build integration

package integration_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/Lauiskk/munchkin/internal/config"
)

// Credenciais do ambiente de teste. São efêmeras: o container nasce e morre
// dentro do teste, e a porta é sorteada pelo Docker.
const (
	testDBName        = "munchkin"
	testOwnerUser     = "munchkin_owner"
	testOwnerPassword = "teste-owner"
	testAppUser       = "munchkin_app"
	testAppPassword   = "teste-app"

	postgresImage = "postgres:18.6-alpine"
)

// startPostgres sobe um PostgreSQL descartável já provisionado com os dois
// papéis do projeto.
//
// Usa o MESMO script de provisionamento do docker compose. Um teste que criasse
// os papéis de outro jeito estaria verificando um banco que não existe em
// lugar nenhum — e a diferença apareceria só em execução, que é tarde.
func startPostgres(t *testing.T) config.DB {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	initScript, err := filepath.Abs(filepath.Join("..", "..", "deploy", "postgres", "10-roles.sh"))
	require.NoError(t, err)

	container, err := tcpostgres.Run(ctx, postgresImage,
		tcpostgres.WithDatabase(testDBName),
		tcpostgres.WithUsername(testOwnerUser),
		tcpostgres.WithPassword(testOwnerPassword),
		tcpostgres.WithInitScripts(initScript),
		testcontainers.WithEnv(map[string]string{
			"MUNCHKIN_APP_USER":     testAppUser,
			"MUNCHKIN_APP_PASSWORD": testAppPassword,
		}),
		// Duas condições, não uma. O log aparece quando o PostgreSQL aceita
		// conexões, mas o mapeamento de porta do Docker pode ainda não existir
		// — e aí MappedPort devolve `port "5432/tcp" not found`, uma falha
		// intermitente que não tem nada a ver com o que o teste verifica.
		testcontainers.WithWaitStrategy(
			wait.ForAll(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2),
				wait.ForListeningPort("5432/tcp"),
			).WithStartupTimeoutDefault(2*time.Minute),
		),
	)
	require.NoError(t, err, "Docker precisa estar em execução para os testes de integração")

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := container.Terminate(ctx); err != nil {
			t.Logf("falha ao encerrar o container: %v", err)
		}
	})

	host, err := container.Host(ctx)
	require.NoError(t, err)
	port, err := container.MappedPort(ctx, "5432/tcp")
	require.NoError(t, err)

	return config.DB{
		Host:            host,
		Port:            int(port.Num()),
		Name:            testDBName,
		User:            testAppUser,
		Password:        config.Secret(testAppPassword),
		SSLMode:         "disable",
		MaxOpenConns:    10,
		MaxIdleConns:    5,
		ConnMaxLifetime: 30 * time.Minute,
		ConnMaxIdleTime: 5 * time.Minute,
		ConnectTimeout:  10 * time.Second,
	}
}

// asOwner devolve a mesma configuração, mas conectando como dono do schema —
// que é quem aplica migrations e cria tabelas.
func asOwner(db config.DB) config.DB {
	db.User = testOwnerUser
	db.Password = config.Secret(testOwnerPassword)
	return db
}

// caminhoDoScriptDePapeis devolve o script de bootstrap dos papéis do banco.
func caminhoDoScriptDePapeis(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "deploy", "postgres", "10-roles.sh"))
	require.NoError(t, err)
	return p
}
