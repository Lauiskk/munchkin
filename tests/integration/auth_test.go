//go:build integration

// Testes de autenticação contra um Keycloak real.
//
// Não há dublê aqui de propósito: o que se está verificando é a integração com
// o IdP — descoberta OIDC, JWKS, assinatura, emissor e audiência. Um dublê
// concordaria com qualquer coisa que eu tivesse entendido errado sobre o
// Keycloak, que é exatamente o erro que este arquivo existe para pegar.
//
// Rode com: make test-integration   (exige `docker compose up -d keycloak`)
package integration_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/internal/adapter/auth"
)

const (
	realm           = "munchkin"
	audience        = "munchkin-api"
	clientProviderA = "provider-a"
	secretProviderA = "local-only-provider-a"
	clientProviderB = "provider-b"
	secretProviderB = "local-only-provider-b"
	clientWalletAdm = "wallet-admin"
	secretWalletAdm = "local-only-wallet-admin"
	clientSemEscopo = "provider-c-sem-escopo"
	secretSemEscopo = "local-only-provider-c"
)

// keycloakBase devolve o IdP que a suíte usa.
//
// Por padrão é o container que ela mesma sobe — foi a última dependência manual
// a cair, e enquanto ela existiu `git clone && make test-integration` não
// funcionava. KEYCLOAK_BASE_URL continua tendo precedência, para quem quiser
// apontar para um IdP já de pé e poupar o tempo de subida.
func keycloakBase() string {
	if v := os.Getenv("KEYCLOAK_BASE_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	base, err := keycloakCompartilhado()
	if err != nil {
		// Devolver vazio faria a falha aparecer como URL malformada muito
		// depois. O pânico aqui é lido por quem roda a suíte, com a causa.
		panic("não foi possível subir o Keycloak da suíte: " + err.Error())
	}
	return base
}

func issuer() string { return keycloakBase() + "/realms/" + realm }

func newKeySet(t *testing.T) *auth.KeySet {
	t.Helper()
	ks := auth.NewKeySet(auth.KeySetOptions{
		Issuer:             issuer(),
		RefreshInterval:    time.Minute,
		MinRefreshInterval: 50 * time.Millisecond,
		HTTPTimeout:        5 * time.Second,
		Logger:             slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	require.NoError(t, ks.Bootstrap(ctx),
		"Keycloak precisa estar de pé: docker compose up -d keycloak")
	return ks
}

func newVerifier(t *testing.T) *auth.Verifier {
	t.Helper()
	return auth.NewVerifier(newKeySet(t), issuer(), audience)
}

// tokenDe obtém um access token real por client_credentials.
func tokenDe(t *testing.T, clientID, secret string) string {
	t.Helper()
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {clientID},
		"client_secret": {secret},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		issuer()+"/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode, "falha ao obter token de %s", clientID)

	var payload struct {
		AccessToken string `json:"access_token"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
	require.NotEmpty(t, payload.AccessToken)
	return payload.AccessToken
}

func TestTokenRealDeProvedorEhAceitoComSuasClaims(t *testing.T) {
	v := newVerifier(t)

	id, err := v.Verify(context.Background(), tokenDe(t, clientProviderA, secretProviderA))
	require.NoError(t, err)

	assert.Equal(t, "provider-a", id.ProviderID, "o provedor vem do token, nunca do corpo")
	assert.Equal(t, clientProviderA, id.ClientID)
	assert.NotEmpty(t, id.Subject)
	assert.True(t, id.IsProvider())
	assert.True(t, id.HasScope(auth.ScopeWageringWrite))
	assert.True(t, id.HasScope(auth.ScopeWageringRead))
	assert.False(t, id.HasScope(auth.ScopeWalletsAdmin),
		"provedor não pode abrir carteira: isso é operação interna")
}

func TestProvedoresDistintosRecebemIdentidadesDistintas(t *testing.T) {
	v := newVerifier(t)

	a, err := v.Verify(context.Background(), tokenDe(t, clientProviderA, secretProviderA))
	require.NoError(t, err)
	b, err := v.Verify(context.Background(), tokenDe(t, clientProviderB, secretProviderB))
	require.NoError(t, err)

	assert.NotEqual(t, a.ProviderID, b.ProviderID)
	assert.Equal(t, "provider-b", b.ProviderID)
}

func TestServicoInternoNaoSeApresentaComoProvedor(t *testing.T) {
	v := newVerifier(t)

	id, err := v.Verify(context.Background(), tokenDe(t, clientWalletAdm, secretWalletAdm))
	require.NoError(t, err)

	assert.Empty(t, id.ProviderID, "o serviço interno não é um provedor")
	assert.False(t, id.IsProvider())
	assert.True(t, id.HasScope(auth.ScopeWalletsAdmin))
	assert.False(t, id.HasScope(auth.ScopeWageringWrite))
}

// Credencial válida não é permissão: autenticado e autorizado são coisas
// diferentes, e o realm traz um cliente que só serve para provar isso.
func TestCredencialValidaSemEscopoEhAutenticadaENaoAutorizada(t *testing.T) {
	v := newVerifier(t)

	id, err := v.Verify(context.Background(), tokenDe(t, clientSemEscopo, secretSemEscopo))
	require.NoError(t, err, "a credencial é válida")

	assert.False(t, id.HasScope(auth.ScopeWageringWrite))
	assert.False(t, id.HasScope(auth.ScopeWageringRead))
	assert.False(t, id.HasScope(auth.ScopeWalletsAdmin))
}

func TestCredencialInvalidaEhRecusadaPeloIdP(t *testing.T) {
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {clientProviderA},
		"client_secret": {"segredo-errado"},
	}
	resp, err := http.Post(issuer()+"/protocol/openid-connect/token",
		"application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// --- Ataques clássicos contra validação de JWT ---------------------------

func TestTokenSemAssinaturaEhRecusado(t *testing.T) {
	v := newVerifier(t)

	// alg "none": o ataque mais antigo do JWT. Uma biblioteca que aceite o
	// algoritmo declarado no cabeçalho engole isto sem reclamar.
	tok := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{
		"iss": issuer(), "aud": audience, "sub": "invasor",
		"exp":   time.Now().Add(time.Hour).Unix(),
		"scope": "wagering:write", "provider_id": "provider-a",
	})
	raw, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	require.NoError(t, err)

	_, err = v.Verify(context.Background(), raw)
	require.Error(t, err)
	assert.ErrorIs(t, err, auth.ErrTokenInvalid)
}

func TestTokenAssinadoPorChaveEstranhaEhRecusado(t *testing.T) {
	v := newVerifier(t)

	chaveDoInvasor, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": issuer(), "aud": audience, "sub": "invasor",
		"exp":   time.Now().Add(time.Hour).Unix(),
		"scope": "wallets:admin", "provider_id": "provider-a",
	})
	// Um kid inexistente força a atualização do JWKS; mesmo assim a chave
	// nunca aparecerá, porque não é do IdP.
	tok.Header["kid"] = "kid-que-nao-existe"
	raw, err := tok.SignedString(chaveDoInvasor)
	require.NoError(t, err)

	_, err = v.Verify(context.Background(), raw)
	require.Error(t, err)
}

func TestTokenExpiradoEhRecusado(t *testing.T) {
	v := newVerifier(t)

	chave, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": issuer(), "aud": audience, "sub": "x",
		"exp": time.Now().Add(-time.Hour).Unix(),
	})
	tok.Header["kid"] = "qualquer"
	expirado, err := tok.SignedString(chave)
	require.NoError(t, err)

	_, err = v.Verify(context.Background(), expirado)
	require.Error(t, err)
}

func TestTokenDeOutroEmissorEhRecusado(t *testing.T) {
	// Verificador configurado para um realm diferente do que emitiu o token.
	ks := newKeySet(t)
	v := auth.NewVerifier(ks, "http://localhost:8180/realms/outro-realm", audience)

	_, err := v.Verify(context.Background(), tokenDe(t, clientProviderA, secretProviderA))
	require.Error(t, err)
	assert.ErrorIs(t, err, auth.ErrTokenInvalid)
}

func TestTokenDeOutraAudienciaEhRecusado(t *testing.T) {
	ks := newKeySet(t)
	v := auth.NewVerifier(ks, issuer(), "outra-api")

	_, err := v.Verify(context.Background(), tokenDe(t, clientProviderA, secretProviderA))
	require.Error(t, err)
	assert.ErrorIs(t, err, auth.ErrTokenInvalid)
}

// A descoberta confere o emissor publicado contra o configurado. Sem isso, uma
// URL trocada por engano faria o serviço validar tokens de outro IdP.
func TestDescobertaRecusaEmissorDivergente(t *testing.T) {
	ks := auth.NewKeySet(auth.KeySetOptions{
		Issuer:       "http://localhost:8180/realms/realm-que-nao-existe",
		DiscoveryURL: issuer() + "/.well-known/openid-configuration",
		HTTPTimeout:  5 * time.Second,
		Logger:       slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})

	err := ks.Bootstrap(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, auth.ErrIssuerMismatch)
}

// A atualização forçada por kid desconhecido tem intervalo mínimo: sem ele,
// tokens com kid aleatório virariam enxurrada de requisições ao IdP.
func TestAtualizacaoForcadaRespeitaIntervaloMinimo(t *testing.T) {
	ks := auth.NewKeySet(auth.KeySetOptions{
		Issuer:             issuer(),
		RefreshInterval:    time.Minute,
		MinRefreshInterval: time.Hour, // efetivamente bloqueia a segunda tentativa
		HTTPTimeout:        5 * time.Second,
		Logger:             slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	require.NoError(t, ks.Bootstrap(context.Background()))

	_, err := ks.KeyFor(context.Background(), "kid-inexistente")
	require.Error(t, err)
	assert.ErrorIs(t, err, auth.ErrKeyNotFound)
	assert.Contains(t, err.Error(), "recente demais",
		"a segunda busca precisa ser barrada pelo intervalo mínimo")
}

func TestBootstrapFalhaQuandoOIdPEstaForaDoAr(t *testing.T) {
	ks := auth.NewKeySet(auth.KeySetOptions{
		Issuer:      "http://127.0.0.1:1/realms/munchkin",
		HTTPTimeout: 2 * time.Second,
		Logger:      slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})

	err := ks.Bootstrap(context.Background())
	require.Error(t, err, "a aplicação não pode subir sem conseguir validar token")
	assert.Contains(t, err.Error(), "descoberta OIDC")
}

// descobrir lê o documento de descoberta do realm.
func descobrir(t *testing.T, base string) map[string]string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		base+"/realms/"+realm+"/.well-known/openid-configuration", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var doc map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&doc))

	campos := map[string]string{}
	for _, k := range []string{"issuer", "jwks_uri", "token_endpoint"} {
		v, ok := doc[k].(string)
		require.True(t, ok, "a descoberta precisa trazer %s", k)
		campos[k] = v
	}
	return campos
}
