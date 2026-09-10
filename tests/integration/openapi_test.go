//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/api"
	adapterauth "github.com/Lauiskk/munchkin/internal/adapter/auth"
	"github.com/Lauiskk/munchkin/internal/adapter/http/handler"
	"github.com/Lauiskk/munchkin/internal/adapter/http/router"
	"github.com/Lauiskk/munchkin/internal/adapter/http/server"
	"github.com/Lauiskk/munchkin/internal/adapter/tracing"
	"github.com/Lauiskk/munchkin/internal/config"
	domainwallet "github.com/Lauiskk/munchkin/internal/domain/wallet"
)

// verificadorFixo devolve sempre a mesma identidade.
//
// A cadeia de middleware é a real — correlação, recover, contexto, log,
// autenticação e escopo. O que se substitui é só a checagem criptográfica do
// token, que já tem teste próprio contra um Keycloak de verdade. Sem esta
// costura, validar as respostas contra o contrato exigiria um IdP de pé para
// cada caso.
type verificadorFixo struct {
	identidade adapterauth.Identity
	err        error
}

func (v verificadorFixo) Verify(context.Context, string) (adapterauth.Identity, error) {
	return v.identidade, v.err
}

// contrato é o documento carregado e validado uma vez.
func contrato(t *testing.T) *openapi3.T {
	t.Helper()
	loader := &openapi3.Loader{IsExternalRefsAllowed: false}
	doc, err := loader.LoadFromData(api.OpenAPI)
	require.NoError(t, err, "o documento embarcado precisa ser um OpenAPI carregável")
	require.NoError(t, doc.Validate(context.Background()),
		"o documento precisa ser um OpenAPI 3 válido")
	return doc
}

type ambienteHTTP struct {
	ambienteExtrato
	doc *openapi3.T
	app func(escopos ...string) *http.Response
}

// aplicacao monta a aplicação HTTP real sobre o ambiente de teste.
func novoAmbienteHTTP(t *testing.T) ambienteHTTP {
	t.Helper()
	return ambienteHTTP{ambienteExtrato: novoAmbienteExtrato(t), doc: contrato(t)}
}

// novoAmbienteHTTPCom monta a aplicação HTTP sobre um banco escolhido.
func novoAmbienteHTTPCom(t *testing.T, dbCfg config.DB) ambienteHTTP {
	t.Helper()
	return ambienteHTTP{ambienteExtrato: novoAmbienteExtratoSobre(t, dbCfg), doc: contrato(t)}
}

// chamar executa uma requisição contra a aplicação real e devolve a resposta.
func (a ambienteHTTP) chamar(
	t *testing.T, metodo, caminho, corpo string, identidade adapterauth.Identity, cabecalhos map[string]string,
) (*http.Response, []byte) {
	t.Helper()
	return a.chamarComVerificador(t, metodo, caminho, corpo,
		verificadorFixo{identidade: identidade}, true, cabecalhos)
}

// chamarComVerificador permite exercitar credencial ausente e inválida.
func (a ambienteHTTP) chamarComVerificador(
	t *testing.T, metodo, caminho, corpo string,
	verificador verificadorFixo, comCabecalho bool, cabecalhos map[string]string,
) (*http.Response, []byte) {
	t.Helper()

	descartado := slog.New(slog.NewTextHandler(io.Discard, nil))
	app := server.New(
		// Prazos reais: RequestTimeout zero criaria um contexto já expirado, e
		// toda chamada ao banco falharia antes de começar.
		config.Config{HTTP: config.HTTP{
			ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second,
			RequestTimeout: 30 * time.Second,
		}},
		descartado,
		handler.NewHealth(descartado, 2*time.Second),
		handler.NewWallet(a.opener, a.getter, a.statement, a.reconciler),
		handler.NewWagering(a.processor, a.querier),
		verificador,
		router.NewPublicPaths(),
		tracing.Nulo(),
	)

	var leitor io.Reader
	if corpo != "" {
		leitor = strings.NewReader(corpo)
	}
	req := httptest.NewRequest(metodo, caminho, leitor)
	if corpo != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if comCabecalho {
		req.Header.Set("Authorization", "Bearer token-de-teste")
	}
	for k, v := range cabecalhos {
		req.Header.Set(k, v)
	}

	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	bruto, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, bruto
}

// conferir valida um corpo real contra o esquema declarado para aquela rota,
// método e status.
//
// É o que impede o contrato de virar ficção: um campo renomeado no DTO, um
// obrigatório que some, um enum novo — tudo isso quebra aqui, e não numa
// integração de terceiro.
func (a ambienteHTTP) conferir(t *testing.T, rota, metodo string, status int, corpo []byte) {
	t.Helper()

	item := a.doc.Paths.Find(rota)
	require.NotNil(t, item, "rota %s não está no contrato", rota)

	op := item.GetOperation(metodo)
	require.NotNil(t, op, "%s %s não está no contrato", metodo, rota)

	resposta := op.Responses.Status(status)
	require.NotNil(t, resposta, "%s %s não declara o status %d", metodo, rota, status)

	media := resposta.Value.Content.Get("application/json")
	require.NotNil(t, media, "%s %s %d não declara corpo JSON", metodo, rota, status)

	var decodificado any
	require.NoError(t, json.Unmarshal(corpo, &decodificado),
		"o corpo devolvido não é JSON: %s", string(corpo))

	require.NoError(t,
		media.Schema.Value.VisitJSON(decodificado, openapi3.MultiErrors()),
		"%s %s %d fugiu do contrato: %s", metodo, rota, status, string(corpo))
}

func admin() adapterauth.Identity {
	return adapterauth.NewIdentity("sub-admin", "wallet-admin", "", "wallets:admin")
}

func provedor() adapterauth.Identity {
	return adapterauth.NewIdentity("sub-a", "provider-a", "provider-a",
		"wagering:write", "wagering:read")
}

// AC-4
func TestDocumentoOpenAPIEhValido(t *testing.T) {
	doc := contrato(t)
	assert.Equal(t, "3.0.3", doc.OpenAPI)
	assert.Len(t, doc.Paths.Map(), 9, "as nove rotas do §9")
}

// AC-5 — as respostas REAIS validam contra os esquemas declarados.
func TestRespostasReaisValidamContraOContrato(t *testing.T) {
	a := novoAmbienteHTTP(t)
	jogador := jogadorNovo(t)

	// Abertura de carteira: 201.
	resp, corpo := a.chamar(t, http.MethodPost, "/wallets",
		fmt.Sprintf(`{"playerId":%q,"initialBalance":{"amount":"100.00","currency":"BRL"}}`, jogador),
		admin(), nil)
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(corpo))
	a.conferir(t, "/wallets", http.MethodPost, http.StatusCreated, corpo)

	var carteira struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(corpo, &carteira))

	// Consulta da carteira: 200.
	resp, corpo = a.chamar(t, http.MethodGet, "/wallets/"+carteira.ID, "", admin(), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	a.conferir(t, "/wallets/{walletId}", http.MethodGet, http.StatusOK, corpo)

	// Operação processada: 200.
	resp, corpo = a.chamar(t, http.MethodPost, "/wagering/transactions",
		operacaoJSON("tx-1", carteira.ID, jogador.String(), "BET", "30.00"),
		provedor(), map[string]string{"Idempotency-Key": "provider-a:tx-1"})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(corpo))
	a.conferir(t, "/wagering/transactions", http.MethodPost, http.StatusOK, corpo)

	var operacao struct {
		TransactionID string `json:"transactionId"`
	}
	require.NoError(t, json.Unmarshal(corpo, &operacao))

	// Recusa de negócio: 422, com failureCode.
	resp, corpo = a.chamar(t, http.MethodPost, "/wagering/transactions",
		operacaoJSON("tx-2", carteira.ID, jogador.String(), "BET", "999.00"),
		provedor(), map[string]string{"Idempotency-Key": "provider-a:tx-2"})
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, string(corpo))
	a.conferir(t, "/wagering/transactions", http.MethodPost, http.StatusUnprocessableEntity, corpo)
	assert.Contains(t, string(corpo), "INSUFFICIENT_FUNDS")

	// Reversão cuja referência ainda não chegou: 202, processamento pendente.
	resp, corpo = a.chamar(t, http.MethodPost, "/wagering/transactions",
		reversaoJSON("rf-1", carteira.ID, jogador.String(), "30.00", "ainda-nao-chegou"),
		provedor(), map[string]string{"Idempotency-Key": "provider-a:rf-1"})
	require.Equal(t, http.StatusAccepted, resp.StatusCode, string(corpo))
	a.conferir(t, "/wagering/transactions", http.MethodPost, http.StatusAccepted, corpo)
	assert.Contains(t, string(corpo), "PENDING_REFERENCE")

	// Detalhe da operação: 200.
	resp, corpo = a.chamar(t, http.MethodGet,
		"/wagering/transactions/"+operacao.TransactionID, "", provedor(), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(corpo))
	a.conferir(t, "/wagering/transactions/{transactionId}", http.MethodGet, http.StatusOK, corpo)

	// Detalhe pela identidade no provedor: 200.
	resp, corpo = a.chamar(t, http.MethodGet,
		"/providers/provider-a/wagering/transactions/tx-1", "", provedor(), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(corpo))
	a.conferir(t, "/providers/{providerId}/wagering/transactions/{externalTransactionId}",
		http.MethodGet, http.StatusOK, corpo)

	// Extrato: 200.
	resp, corpo = a.chamar(t, http.MethodGet,
		"/wallets/"+carteira.ID+"/ledger?limit=1", "", admin(), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(corpo))
	a.conferir(t, "/wallets/{walletId}/ledger", http.MethodGet, http.StatusOK, corpo)

	// Reconciliação: 200.
	resp, corpo = a.chamar(t, http.MethodPost,
		"/wallets/"+carteira.ID+"/reconciliation", "", admin(), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(corpo))
	a.conferir(t, "/wallets/{walletId}/reconciliation", http.MethodPost, http.StatusOK, corpo)

	// Vivacidade e prontidão: públicas.
	resp, corpo = a.chamar(t, http.MethodGet, "/health/live", "", adapterauth.Identity{}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	a.conferir(t, "/health/live", http.MethodGet, http.StatusOK, corpo)
}

// AC-5 para os corpos de erro — que são o que mais some da documentação.
func TestCorposDeErroValidamContraOContrato(t *testing.T) {
	a := novoAmbienteHTTP(t)
	inexistente, err := domainwallet.NewID()
	require.NoError(t, err)

	casos := []struct {
		nome, metodo, caminho, rota, corpo string
		identidade                         adapterauth.Identity
		status                             int
		cabecalhos                         map[string]string
	}{
		{
			nome: "entrada inválida", metodo: http.MethodPost, caminho: "/wallets",
			rota: "/wallets", corpo: `{"playerId":"não-é-uuid","initialBalance":{"amount":"x","currency":"ZZZ"}}`,
			identidade: admin(), status: http.StatusBadRequest,
		},
		{
			nome: "sem o escopo exigido", metodo: http.MethodGet,
			caminho: "/wallets/" + inexistente.String(), rota: "/wallets/{walletId}",
			identidade: provedor(), status: http.StatusForbidden,
		},
		{
			nome: "carteira inexistente", metodo: http.MethodGet,
			caminho: "/wallets/" + inexistente.String(), rota: "/wallets/{walletId}",
			identidade: admin(), status: http.StatusNotFound,
		},
		{
			nome: "extrato de carteira inexistente", metodo: http.MethodGet,
			caminho:    "/wallets/" + inexistente.String() + "/ledger",
			rota:       "/wallets/{walletId}/ledger",
			identidade: admin(), status: http.StatusNotFound,
		},
		{
			nome: "cursor corrompido", metodo: http.MethodGet,
			caminho:    "/wallets/" + inexistente.String() + "/ledger?cursor=xxx",
			rota:       "/wallets/{walletId}/ledger",
			identidade: admin(), status: http.StatusBadRequest,
		},
	}

	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			resp, corpo := a.chamar(t, c.metodo, c.caminho, c.corpo, c.identidade, c.cabecalhos)
			require.Equal(t, c.status, resp.StatusCode, string(corpo))
			a.conferir(t, c.rota, c.metodo, c.status, corpo)
		})
	}
}

// AC-8 — os cinco cenários do §9 estão documentados na rota de operação.
func TestOsCincoCenariosDoEnunciadoEstaoNoContrato(t *testing.T) {
	doc := contrato(t)
	op := doc.Paths.Find("/wagering/transactions").GetOperation(http.MethodPost)
	require.NotNil(t, op)

	for _, status := range []int{200, 202, 400, 409, 422, 503} {
		assert.NotNil(t, op.Responses.Status(status),
			"o §9 exige que o status %d seja distinguível pelo contrato", status)
	}
}

func operacaoJSON(externo, carteira, jogador, kind, valor string) string {
	return fmt.Sprintf(`{"providerId":"provider-a","externalTransactionId":%q,
	  "playerId":%q,"walletId":%q,"roundId":"round-1","gameId":"fortune-chimp",
	  "kind":%q,"money":{"amount":%q,"currency":"BRL"}}`,
		externo, jogador, carteira, kind, valor)
}

func reversaoJSON(externo, carteira, jogador, valor, referencia string) string {
	return fmt.Sprintf(`{"providerId":"provider-a","externalTransactionId":%q,
	  "playerId":%q,"walletId":%q,"roundId":"round-1","gameId":"fortune-chimp",
	  "kind":"REFUND","money":{"amount":%q,"currency":"BRL"},
	  "referenceExternalTransactionId":%q}`,
		externo, jogador, carteira, valor, referencia)
}

// jsonDe decodifica um corpo de resposta.
func jsonDe(corpo []byte, destino any) error { return json.Unmarshal(corpo, destino) }
