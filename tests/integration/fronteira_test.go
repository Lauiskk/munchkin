//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/internal/domain/money"
	domainwallet "github.com/Lauiskk/munchkin/internal/domain/wallet"
)

// A fronteira HTTP não tinha um único teste próprio: nem `handler`, nem
// `middleware`, nem `router`, nem `dto.Bind`.
//
// Uma passada de QA exercitou 40 casos à mão contra a pilha viva e todos
// passaram — a fronteira estava certa, só desprotegida. Este arquivo fixa os
// que não são alcançados por nenhuma outra camada: o que o parser de corpo
// recusa, o cabeçalho de idempotência, o identificador malformado no caminho e
// o 409, que a suíte declarava no contrato sem nunca ter produzido um.
//
// Os casos de formato de valor monetário ficaram de fora de propósito: o parser
// já tem 24 casos em unidade mais fuzzing, e repeti-los por HTTP mediria a
// mesma coisa duas vezes.

// corpoDeErro é o envelope que toda falha devolve.
type corpoDeErro struct {
	Code          string `json:"code"`
	Message       string `json:"message"`
	CorrelationID string `json:"correlationId"`
	Fields        []struct {
		Field string `json:"field"`
	} `json:"fields"`
}

func erroDe(t *testing.T, bruto []byte) corpoDeErro {
	t.Helper()
	var e corpoDeErro
	require.NoError(t, json.Unmarshal(bruto, &e), "corpo devolvido: %s", string(bruto))
	return e
}

// TestCorpoQueNaoDaParaInterpretarEhRecusadoNaFronteira cobre o dto.Bind.
func TestCorpoQueNaoDaParaInterpretarEhRecusadoNaFronteira(t *testing.T) {
	a := novoAmbienteHTTP(t)
	w, jogador := a.carteiraCom(t, "100.00")

	// 16 KiB é o teto do Bind. O excedente tem de ser recusado ANTES de
	// qualquer validação de negócio: aceitar corpo maior deixaria o cliente
	// decidir quanta memória o processo gasta antes de saber o que chegou.
	gordo := fmt.Sprintf(
		`{"providerId":"provider-a","externalTransactionId":"gordo","playerId":%q,
		  "walletId":%q,"roundId":"r","gameId":"g","kind":"BET",
		  "money":{"amount":"1.00","currency":"BRL"},"sobra":%q}`,
		jogador, w, strings.Repeat("y", 20000))

	casos := []struct {
		nome, corpo string
	}{
		{"corpo ausente", ""},
		{"JSON truncado", `{"providerId":`},
		{"JSON que não é objeto", `["provider-a"]`},
		{"corpo acima de 16 KiB", gordo},
	}

	for _, caso := range casos {
		t.Run(caso.nome, func(t *testing.T) {
			resp, bruto := a.chamar(t, http.MethodPost, "/wagering/transactions", caso.corpo,
				provedor(), map[string]string{"Idempotency-Key": "provider-a:corpo"})

			require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(bruto))
			e := erroDe(t, bruto)
			assert.Contains(t, []string{"MALFORMED_REQUEST", "VALIDATION_ERROR"}, e.Code)
			assert.NotEmpty(t, e.CorrelationID, "falha sem correlação não se investiga")
			a.conferir(t, "/wagering/transactions", http.MethodPost, resp.StatusCode, bruto)
		})
	}
}

// TestCabecalhoDeIdempotenciaEhExigidoEValidado cobre o cabeçalho.
//
// Ele é lido ANTES do corpo, e é o único campo obrigatório que não viaja no
// JSON — por isso escapava de toda validação já testada.
func TestCabecalhoDeIdempotenciaEhExigidoEValidado(t *testing.T) {
	a := novoAmbienteHTTP(t)
	w, jogador := a.carteiraCom(t, "100.00")
	corpo := operacaoJSON("chave-1", w.String(), jogador.String(), "BET", "10.00")

	casos := map[string]map[string]string{
		"ausente":                nil,
		"vazio":                  {"Idempotency-Key": ""},
		"com espaço no meio":     {"Idempotency-Key": "chave com espaço"},
		"acima de 128 chars":     {"Idempotency-Key": strings.Repeat("k", 129)},
		"com caractere proibido": {"Idempotency-Key": "provider-a:x/y"},
	}

	for nome, cabecalhos := range casos {
		t.Run(nome, func(t *testing.T) {
			resp, bruto := a.chamar(t, http.MethodPost, "/wagering/transactions",
				corpo, provedor(), cabecalhos)

			require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(bruto))
			e := erroDe(t, bruto)
			assert.Equal(t, "VALIDATION_ERROR", e.Code)
			require.NotEmpty(t, e.Fields, "o erro precisa apontar o campo")
			assert.Equal(t, "Idempotency-Key", e.Fields[0].Field)
			a.conferir(t, "/wagering/transactions", http.MethodPost, resp.StatusCode, bruto)
		})
	}

	// E a chave válida passa, para o teste provar que a recusa vinha do
	// cabeçalho e não de outra coisa do corpo.
	resp, bruto := a.chamar(t, http.MethodPost, "/wagering/transactions", corpo,
		provedor(), map[string]string{"Idempotency-Key": "provider-a:chave-1"})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(bruto))

	// A regra de "sem espaço nas bordas" do domínio é INALCANÇÁVEL por
	// cabeçalho: o parser HTTP descarta o espaço em volta do valor antes de
	// qualquer código nosso enxergá-lo — o OWS não faz parte do valor
	// (RFC 7230 §3.2.4). A regra não é inútil: ela guarda o caminho da
	// MENSAGERIA, onde a chave viaja dentro do JSON e o espaço sobrevive.
	//
	// A consequência observável, que é o que este caso fixa: a mesma chave com
	// bordas é a MESMA chave, então o reenvio é replay e não operação nova.
	resp, bruto = a.chamar(t, http.MethodPost, "/wagering/transactions", corpo,
		provedor(), map[string]string{"Idempotency-Key": "  provider-a:chave-1  "})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(bruto))
	var replay struct {
		IdempotentReplay bool `json:"idempotentReplay"`
	}
	require.NoError(t, json.Unmarshal(bruto, &replay))
	assert.True(t, replay.IdempotentReplay,
		"o cabeçalho chega sem as bordas: é a mesma chave, e o reenvio é replay")
}

// TestIdentificadorMalformadoNoCaminhoEhQuatrocentosENaoQuinhentos guarda os
// path params.
//
// Um UUID inválido no caminho é entrada do cliente, e responder 500 a ela
// convidaria quem sonda o serviço a procurar mais: 500 sugere que algo se
// quebrou por dentro, 400 diz que a requisição estava errada.
func TestIdentificadorMalformadoNoCaminhoEhQuatrocentosENaoQuinhentos(t *testing.T) {
	a := novoAmbienteHTTP(t)

	casos := []struct{ nome, metodo, caminho, rota string }{
		{"carteira", http.MethodGet, "/wallets/nao-e-uuid", "/wallets/{walletId}"},
		{"extrato", http.MethodGet, "/wallets/nao-e-uuid/ledger", "/wallets/{walletId}/ledger"},
		{"reconciliação", http.MethodPost, "/wallets/nao-e-uuid/reconciliation",
			"/wallets/{walletId}/reconciliation"},
	}
	for _, caso := range casos {
		t.Run(caso.nome, func(t *testing.T) {
			resp, bruto := a.chamar(t, caso.metodo, caso.caminho, "", admin(), nil)
			require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(bruto))
			assert.NotEmpty(t, erroDe(t, bruto).CorrelationID)
			a.conferir(t, caso.rota, caso.metodo, resp.StatusCode, bruto)
		})
	}

	t.Run("operação", func(t *testing.T) {
		resp, bruto := a.chamar(t, http.MethodGet, "/wagering/transactions/nao-e-uuid",
			"", provedor(), nil)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(bruto))
		a.conferir(t, "/wagering/transactions/{transactionId}", http.MethodGet,
			resp.StatusCode, bruto)
	})
}

// TestConflitoDeIdempotenciaSaiComoQuatrocentosENove produz o 409 de verdade.
//
// O contrato o declarava e a suíte conferia a declaração; nenhuma resposta 409
// real havia sido produzida nem validada. As duas formas de conflito saem daqui
// com o mesmo código, e nenhuma delas encosta no saldo.
func TestConflitoDeIdempotenciaSaiComoQuatrocentosENove(t *testing.T) {
	a := novoAmbienteHTTP(t)
	w, jogador := a.carteiraCom(t, "100.00")
	chave := map[string]string{"Idempotency-Key": "provider-a:conflito"}

	resp, bruto := a.chamar(t, http.MethodPost, "/wagering/transactions",
		operacaoJSON("conflito", w.String(), jogador.String(), "BET", "10.00"), provedor(), chave)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(bruto))

	t.Run("mesma chave, conteúdo diferente", func(t *testing.T) {
		resp, bruto := a.chamar(t, http.MethodPost, "/wagering/transactions",
			operacaoJSON("conflito", w.String(), jogador.String(), "BET", "11.00"), provedor(), chave)

		require.Equal(t, http.StatusConflict, resp.StatusCode, string(bruto))
		e := erroDe(t, bruto)
		assert.Equal(t, "CONFLICT", e.Code)
		assert.NotEmpty(t, e.CorrelationID)
		a.conferir(t, "/wagering/transactions", http.MethodPost, resp.StatusCode, bruto)
	})

	t.Run("mesma operação, outra chave", func(t *testing.T) {
		resp, bruto := a.chamar(t, http.MethodPost, "/wagering/transactions",
			operacaoJSON("conflito", w.String(), jogador.String(), "BET", "10.00"),
			provedor(), map[string]string{"Idempotency-Key": "provider-a:outra"})

		require.Equal(t, http.StatusConflict, resp.StatusCode, string(bruto))
		assert.Equal(t, "CONFLICT", erroDe(t, bruto).Code)
	})

	// Um débito só: nenhum dos conflitos moveu dinheiro.
	assert.Equal(t, "90.00 BRL", a.saldo(t, w))
}

// TestRecusaSemRegistroNaoAncoraIdempotencia fixa uma semântica que estava
// implementada e não estava documentada nem testada.
//
// Recusa por alvo inválido — carteira inexistente, jogador errado, moeda
// divergente, valor incompatível com o tipo — NÃO grava linha. Sem linha não há
// o que reproduzir, então a mesma chave volta a ser avaliada do zero e pode
// terminar diferente se o mundo tiver mudado.
//
// É deliberado, e o contraste importa: a recusa por SALDO é persistida, e o
// replay dela continua recusando mesmo depois de a carteira ser creditada.
// Recusa que teve efeito nenhum não é fato a preservar; recusa que decidiu
// sobre dinheiro é.
func TestRecusaSemRegistroNaoAncoraIdempotencia(t *testing.T) {
	a := novoAmbienteHTTP(t)
	chave := map[string]string{"Idempotency-Key": "provider-a:fantasma"}

	inexistente, err := domainwallet.NewID()
	require.NoError(t, err)
	jogador := jogadorNovo(t)
	corpo := operacaoJSON("fantasma", inexistente.String(), jogador.String(), "BET", "10.00")

	resp, bruto := a.chamar(t, http.MethodPost, "/wagering/transactions", corpo, provedor(), chave)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, string(bruto))
	var recusa struct {
		Status        string `json:"status"`
		FailureCode   string `json:"failureCode"`
		TransactionID string `json:"transactionId"`
	}
	require.NoError(t, json.Unmarshal(bruto, &recusa))
	assert.Equal(t, "WALLET_NOT_FOUND", recusa.FailureCode)
	assert.Empty(t, recusa.TransactionID, "recusa sem registro não tem identidade a devolver")

	// A recusa por saldo, ao contrário, é gravada — e o replay dela devolve o
	// desfecho persistido mesmo com a carteira já creditada.
	w, dono := a.carteiraCom(t, "10.00")
	comSaldo := map[string]string{"Idempotency-Key": "provider-a:sem-saldo"}
	caro := operacaoJSON("sem-saldo", w.String(), dono.String(), "BET", "500.00")

	resp, bruto = a.chamar(t, http.MethodPost, "/wagering/transactions", caro, provedor(), comSaldo)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, string(bruto))

	resp, bruto = a.chamar(t, http.MethodPost, "/wagering/transactions",
		operacaoJSON("credito", w.String(), dono.String(), "WIN", "9000.00"),
		provedor(), map[string]string{"Idempotency-Key": "provider-a:credito"})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(bruto))

	resp, bruto = a.chamar(t, http.MethodPost, "/wagering/transactions", caro, provedor(), comSaldo)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode,
		"o replay não pode reavaliar: a recusa por saldo é fato registrado")
	var replay struct {
		FailureCode      string `json:"failureCode"`
		IdempotentReplay bool   `json:"idempotentReplay"`
	}
	require.NoError(t, json.Unmarshal(bruto, &replay))
	assert.Equal(t, "INSUFFICIENT_FUNDS", replay.FailureCode)
	assert.True(t, replay.IdempotentReplay)
	assert.Equal(t, "9010.00 BRL", a.saldo(t, w), "nenhum dos envios debitou")
}

// TestGanhoAceitaReferenciaDaApostaDaMesmaRodada guarda a regra do §7.
//
// O enunciado diz que `WIN` **pode** informar uma aposta da mesma rodada como
// referência. O sistema recusava, e o erro virava HTTP 500 — uma entrada que o
// enunciado declara legítima tratada como defeito do servidor. Havia até um
// teste de domínio afirmando a recusa como se fosse o certo.
//
// A referência no ganho é INFORMATIVA: fica gravada e não é resolvida. O crédito
// é imediato mesmo quando a aposta referenciada ainda não existe — um ganho que
// esperasse pela aposta deixaria de ser crédito.
func TestGanhoAceitaReferenciaDaApostaDaMesmaRodada(t *testing.T) {
	a := novoAmbienteHTTP(t)
	w, jogador := a.carteiraCom(t, "100.00")

	comReferencia := func(externo, kind, valor, referencia string) string {
		return fmt.Sprintf(`{"providerId":"provider-a","externalTransactionId":%q,
		  "playerId":%q,"walletId":%q,"roundId":"round-1","gameId":"fortune-chimp",
		  "kind":%q,"money":{"amount":%q,"currency":"BRL"},
		  "referenceExternalTransactionId":%q}`,
			externo, jogador, w, kind, valor, referencia)
	}

	// A aposta da rodada, para o ganho ter o que referenciar.
	resp, bruto := a.chamar(t, http.MethodPost, "/wagering/transactions",
		operacaoJSON("aposta-r1", w.String(), jogador.String(), "BET", "30.00"),
		provedor(), map[string]string{"Idempotency-Key": "provider-a:aposta-r1"})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(bruto))

	resp, bruto = a.chamar(t, http.MethodPost, "/wagering/transactions",
		comReferencia("ganho-r1", "WIN", "45.00", "aposta-r1"),
		provedor(), map[string]string{"Idempotency-Key": "provider-a:ganho-r1"})

	require.Equal(t, http.StatusOK, resp.StatusCode, string(bruto))
	var out struct {
		Status        string `json:"status"`
		TransactionID string `json:"transactionId"`
		Balance       struct {
			Amount string `json:"amount"`
		} `json:"balance"`
	}
	require.NoError(t, json.Unmarshal(bruto, &out))
	assert.Equal(t, "PROCESSED", out.Status, "o ganho credita na hora, não vira pendência")
	assert.Equal(t, "115.00", out.Balance.Amount, "100 - 30 + 45")
	a.conferir(t, "/wagering/transactions", http.MethodPost, resp.StatusCode, bruto)

	// A referência ficou GRAVADA e NÃO foi resolvida: reference_transaction_id
	// permanece nulo, que é o que separa "informa" de "desfaz".
	var externa, interna *string
	require.NoError(t, a.db.Session(a.ctx).Raw(
		`SELECT reference_external_transaction_id, reference_transaction_id::text
		   FROM wager_transactions WHERE id = ?`, out.TransactionID).
		Row().Scan(&externa, &interna))
	require.NotNil(t, externa)
	assert.Equal(t, "aposta-r1", *externa)
	assert.Nil(t, interna, "o ganho grava a referência, não a resolve")

	// E um ganho cuja aposta ainda não chegou credita do mesmo jeito: a
	// referência não é condição, é anotação.
	resp, bruto = a.chamar(t, http.MethodPost, "/wagering/transactions",
		comReferencia("ganho-orfao", "WIN", "10.00", "aposta-que-nunca-chegou"),
		provedor(), map[string]string{"Idempotency-Key": "provider-a:ganho-orfao"})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(bruto))
	require.NoError(t, json.Unmarshal(bruto, &out))
	assert.Equal(t, "PROCESSED", out.Status,
		"referência ausente não pode segurar um crédito")
	assert.Equal(t, "125.00", out.Balance.Amount)
}

// TestReferenciaOndeNaoCabeEhQuatrocentosENaoQuinhentos completa a regra.
//
// `BET` e `LOSS` não têm o que referenciar. A recusa acontece na fronteira, com
// o campo apontado — e não no domínio, de onde saía um erro genérico que o
// tradutor não conhecia e transformava em 500.
func TestReferenciaOndeNaoCabeEhQuatrocentosENaoQuinhentos(t *testing.T) {
	a := novoAmbienteHTTP(t)
	w, jogador := a.carteiraCom(t, "100.00")

	casos := []struct{ kind, valor string }{{"BET", "30.00"}, {"LOSS", "0.00"}}
	for _, caso := range casos {
		t.Run(caso.kind+" com referência", func(t *testing.T) {
			corpo := fmt.Sprintf(`{"providerId":"provider-a","externalTransactionId":"ref-%s",
			  "playerId":%q,"walletId":%q,"roundId":"round-1","gameId":"g","kind":%q,
			  "money":{"amount":%q,"currency":"BRL"},
			  "referenceExternalTransactionId":"aposta-original"}`,
				caso.kind, jogador, w, caso.kind, caso.valor)

			resp, bruto := a.chamar(t, http.MethodPost, "/wagering/transactions", corpo,
				provedor(), map[string]string{"Idempotency-Key": "provider-a:ref-" + caso.kind})

			require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(bruto))
			e := erroDe(t, bruto)
			assert.Equal(t, "VALIDATION_ERROR", e.Code)
			require.NotEmpty(t, e.Fields)
			assert.Equal(t, "referenceExternalTransactionId", e.Fields[0].Field)
			a.conferir(t, "/wagering/transactions", http.MethodPost, resp.StatusCode, bruto)
		})
	}

	// E a reversão continua EXIGINDO a referência.
	corpo := fmt.Sprintf(`{"providerId":"provider-a","externalTransactionId":"refund-sem-ref",
	  "playerId":%q,"walletId":%q,"roundId":"round-1","gameId":"g","kind":"REFUND",
	  "money":{"amount":"30.00","currency":"BRL"}}`, jogador, w)
	resp, bruto := a.chamar(t, http.MethodPost, "/wagering/transactions", corpo,
		provedor(), map[string]string{"Idempotency-Key": "provider-a:refund-sem-ref"})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(bruto))
}

// TestAberturaInternaNaoEntraPorHTTP fecha a outra porta.
//
// `OPENING` é reservado à abertura interna: um provedor que pudesse enviá-la
// creditaria a própria carteira. Pela fila havia teste; por HTTP não havia
// nenhum, e trocar `ParseExternalKind` por `ParseKind` na decodificação passaria
// a suíte inteira verde — restaria só a constraint do banco, com 500 no lugar de
// 400.
func TestAberturaInternaNaoEntraPorHTTP(t *testing.T) {
	a := novoAmbienteHTTP(t)
	w, jogador := a.carteiraCom(t, "100.00")

	resp, bruto := a.chamar(t, http.MethodPost, "/wagering/transactions",
		operacaoJSON("abertura-pirata", w.String(), jogador.String(), "OPENING", "500.00"),
		provedor(), map[string]string{"Idempotency-Key": "provider-a:abertura-pirata"})

	require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(bruto))
	e := erroDe(t, bruto)
	assert.Equal(t, "VALIDATION_ERROR", e.Code)
	require.NotEmpty(t, e.Fields)
	assert.Equal(t, "kind", e.Fields[0].Field)
	a.conferir(t, "/wagering/transactions", http.MethodPost, resp.StatusCode, bruto)

	assert.Equal(t, "100.00 BRL", a.saldo(t, w), "nenhum crédito escapou")
}

// TestReplayDeRecusaNaoInventaSaldo guarda o que o §9 quer dizer com "resultado
// persistido".
//
// A recusa original devolve o saldo observado no momento em que ela aconteceu,
// lido sob o lock da carteira. Esse saldo NÃO é persistido: só operação
// concluída guarda resultado financeiro, e a constraint do banco garante isso.
//
// No replay, o código antes fabricava um zero para preencher o campo — e zero
// num campo de saldo não é "não há", é "a carteira está vazia". A mesma chave,
// com o mesmo desfecho, devolvia 100,00 na primeira vez e 0,00 na segunda.
//
// Agora o campo simplesmente não vai. Encontrado testando a plataforma de ponta
// a ponta contra o enunciado.
func TestReplayDeRecusaNaoInventaSaldo(t *testing.T) {
	a := novoAmbienteHTTP(t)
	w, jogador := a.carteiraCom(t, "100.00")
	chave := map[string]string{"Idempotency-Key": "provider-a:sem-saldo"}
	corpo := operacaoJSON("sem-saldo", w.String(), jogador.String(), "BET", "5000.00")

	type resposta struct {
		Status           string          `json:"status"`
		FailureCode      string          `json:"failureCode"`
		IdempotentReplay bool            `json:"idempotentReplay"`
		Balance          *money.Money    `json:"balance"`
		Cru              json.RawMessage `json:"-"`
	}
	enviar := func(t *testing.T) (resposta, []byte) {
		t.Helper()
		resp, bruto := a.chamar(t, http.MethodPost, "/wagering/transactions", corpo, provedor(), chave)
		require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, string(bruto))
		var r resposta
		require.NoError(t, json.Unmarshal(bruto, &r))
		return r, bruto
	}

	original, _ := enviar(t)
	require.Equal(t, "INSUFFICIENT_FUNDS", original.FailureCode)
	require.False(t, original.IdempotentReplay)
	require.NotNil(t, original.Balance, "a recusa informa o saldo que ela observou")
	assert.Equal(t, "100.00", original.Balance.Amount())

	replay, bruto := enviar(t)
	assert.True(t, replay.IdempotentReplay)
	assert.Equal(t, original.Status, replay.Status, "o desfecho é o mesmo")
	assert.Equal(t, original.FailureCode, replay.FailureCode)
	assert.Nil(t, replay.Balance,
		"não há saldo persistido numa recusa: o campo tem de sumir, não virar zero")
	assert.NotContains(t, string(bruto), `"balance"`,
		"zero num campo de saldo afirma que a carteira está vazia, e isso é falso")

	// E o contrato continua descrevendo o que saiu.
	a.conferir(t, "/wagering/transactions", http.MethodPost, http.StatusUnprocessableEntity, bruto)
}
