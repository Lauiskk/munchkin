//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const keycloakImage = "quay.io/keycloak/keycloak:26.7.3"

var (
	keycloakUmaVez sync.Once
	keycloakURL    string
	keycloakErr    error
	keycloakParar  func()
)

// keycloakCompartilhado sobe o IdP da suíte e devolve a URL base.
//
// O realm é o MESMO que o compose importa, versionado no repositório. Um realm
// próprio de teste divergiria do que roda em desenvolvimento, e a divergência
// apareceria como teste verde sobre configuração que ninguém usa.
//
// Sem `KC_HOSTNAME` fixado, e com `KC_HOSTNAME_STRICT=false`, o Keycloak deriva
// o emissor do host da requisição. Com porta aleatória isso é exatamente o que
// se quer: o emissor e o `jwks_uri` da descoberta já saem alcançáveis, sem a
// reescrita que o compose precisa — lá o emissor é fixado de propósito, para
// ser estável entre a rede interna e o host.
func keycloakCompartilhado() (string, error) {
	keycloakUmaVez.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		realm, err := filepath.Abs(filepath.Join("..", "..", "deploy", "keycloak", "realm-munchkin.json"))
		if err != nil {
			keycloakErr = err
			return
		}

		container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
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
				// MONTADO, não copiado. O diretório de import não existe na
				// imagem, e copiar para dentro dele falha com "não encontrei o
				// caminho" — o realm nunca é importado, a descoberta responde
				// 404 e a espera estoura o prazo sem dizer por quê. A montagem
				// cria o diretório, que é como o compose já fazia.
				HostConfigModifier: func(hc *container.HostConfig) {
					hc.Binds = append(hc.Binds,
						realm+":/opt/keycloak/data/import/realm-munchkin.json:ro")
				},
				WaitingFor: wait.ForListeningPort("8080/tcp").
					WithStartupTimeout(3 * time.Minute),
			},
		})
		if err != nil {
			keycloakErr = fmt.Errorf("Docker precisa estar em execução: %w", err)
			return
		}
		keycloakParar = func() {
			c, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			_ = container.Terminate(c)
		}

		host, err := container.Host(ctx)
		if err != nil {
			keycloakErr = err
			return
		}
		porta, err := container.MappedPort(ctx, "8080/tcp")
		if err != nil {
			keycloakErr = err
			return
		}
		keycloakURL = "http://" + host + ":" + porta.Port()

		// A porta abre bem ANTES de o realm terminar de importar, e um teste
		// que começasse ali receberia 404 no lugar da configuração. A sondagem
		// é feita aqui, e não pela espera do testcontainers, porque a sonda
		// HTTP dela não enxergou este container — e um prazo estourando sem
		// dizer por quê custa mais caro que quatro linhas explícitas.
		keycloakErr = esperarRealm(ctx, keycloakURL)
	})
	return keycloakURL, keycloakErr
}

// AC-1, AC-2 — o IdP da suíte é real e se descreve corretamente.
func TestIdPDaSuiteSobeSozinhoESeDescreveCorretamente(t *testing.T) {
	base, err := keycloakCompartilhado()
	require.NoError(t, err)
	require.Equal(t, base, keycloakBase(),
		"os testes de autenticação precisam usar este container, e não um Keycloak externo")

	descoberta := descobrir(t, base)

	require.Equal(t, base+"/realms/"+realm, descoberta["issuer"],
		"o emissor tem de ser o do container: com porta aleatória, um emissor fixo "+
			"tornaria o jwks_uri inalcançável — foi assim que isso quebrou no compose")
	require.Contains(t, descoberta["jwks_uri"], base,
		"o jwks_uri informado pela descoberta precisa ser alcançável de onde se lê")
}

// esperarRealm sonda a descoberta até o realm existir.
func esperarRealm(ctx context.Context, base string) error {
	alvo := base + "/realms/" + realm + "/.well-known/openid-configuration"
	cliente := &http.Client{Timeout: 5 * time.Second}

	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, alvo, nil)
		if err != nil {
			return err
		}
		resp, err := cliente.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("o realm %q não ficou disponível em %s: %w", realm, base, ctx.Err())
		case <-time.After(time.Second):
		}
	}
}
