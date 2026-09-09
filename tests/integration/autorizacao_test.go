//go:build integration

package integration_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	adapterauth "github.com/Lauiskk/munchkin/internal/adapter/auth"
)

// AC-7, AC-8 — acesso não autorizado não pode ter EFEITO.
//
// O §13 pede "ausência de efeitos financeiros" nesses acessos, e isso é
// diferente de devolver o código certo: um 403 entregue DEPOIS de o saldo mudar
// seria pior que um 200. Por isso o teste confere o saldo, a versão da carteira
// e o número de lançamentos — não a resposta.
func TestAcessoNaoAutorizadoNaoMoveDinheiro(t *testing.T) {
	a := novoAmbienteHTTP(t)
	jogador := jogadorNovo(t)

	_, corpo := a.chamar(t, http.MethodPost, "/wallets",
		fmt.Sprintf(`{"playerId":%q,"initialBalance":{"amount":"100.00","currency":"BRL"}}`, jogador),
		admin(), nil)
	var carteira struct {
		ID string `json:"id"`
	}
	require.NoError(t, jsonDe(corpo, &carteira))

	id, err := uuid.Parse(carteira.ID)
	require.NoError(t, err)
	antes := a.instantaneoPorUUID(t, id)

	aposta := operacaoJSON("tx-invasor", carteira.ID, jogador.String(), "BET", "50.00")
	cabecalhos := map[string]string{"Idempotency-Key": "provider-a:tx-invasor"}

	casos := map[string]struct {
		verificador  verificadorFixo
		comCabecalho bool
		status       int
	}{
		"sem credencial": {
			verificador: verificadorFixo{}, comCabecalho: false, status: http.StatusUnauthorized,
		},
		"credencial inválida": {
			verificador:  verificadorFixo{err: errors.New("assinatura não confere")},
			comCabecalho: true, status: http.StatusUnauthorized,
		},
		// O provedor É o do corpo — só falta o escopo de escrita. Sem este
		// caso, todos os demais seriam barrados pela conferência de provedor
		// antes de o escopo importar, e o teste passaria com RequireScope
		// desligado. Verificado por mutação.
		"provedor certo, sem o escopo de escrita": {
			verificador: verificadorFixo{
				identidade: adapterauth.NewIdentity("sub-a", "provider-a", "provider-a", "wagering:read"),
			},
			comCabecalho: true, status: http.StatusForbidden,
		},
		"outro provedor, com escopo": {
			verificador: verificadorFixo{
				identidade: adapterauth.NewIdentity("sub-c", "provider-c", "provider-c", "wagering:write"),
			},
			comCabecalho: true, status: http.StatusForbidden,
		},
		"escopo de outra coisa": {
			verificador:  verificadorFixo{identidade: admin()},
			comCabecalho: true, status: http.StatusForbidden,
		},
	}

	for nome, c := range casos {
		t.Run(nome, func(t *testing.T) {
			resp, corpo := a.chamarComVerificador(t, http.MethodPost, "/wagering/transactions",
				aposta, c.verificador, c.comCabecalho, cabecalhos)

			require.Equal(t, c.status, resp.StatusCode, string(corpo))
			assert.Equal(t, antes, a.instantaneoPorUUID(t, id),
				"saldo, versão e lançamentos têm de ficar exatamente como estavam")
			assert.Zero(t, a.conta(t, `SELECT count(*) FROM wager_transactions
				WHERE external_transaction_id = 'tx-invasor'`),
				"a operação não pode nem chegar a ser registrada")
		})
	}
}

// AC-9 — consulta de outro provedor não expõe dado nem existência.
func TestConsultaDeOutroProvedorNaoExpoeNada(t *testing.T) {
	a := novoAmbienteHTTP(t)
	jogador := jogadorNovo(t)

	_, corpo := a.chamar(t, http.MethodPost, "/wallets",
		fmt.Sprintf(`{"playerId":%q,"initialBalance":{"amount":"100.00","currency":"BRL"}}`, jogador),
		admin(), nil)
	var carteira struct {
		ID string `json:"id"`
	}
	require.NoError(t, jsonDe(corpo, &carteira))

	_, corpo = a.chamar(t, http.MethodPost, "/wagering/transactions",
		operacaoJSON("tx-do-a", carteira.ID, jogador.String(), "BET", "30.00"),
		provedor(), map[string]string{"Idempotency-Key": "provider-a:tx-do-a"})
	var operacao struct {
		TransactionID string `json:"transactionId"`
	}
	require.NoError(t, jsonDe(corpo, &operacao))

	outro := verificadorFixo{
		identidade: adapterauth.NewIdentity("sub-b", "provider-b", "provider-b",
			"wagering:read", "wagering:write"),
	}

	for nome, caminho := range map[string]string{
		"pela identidade interna": "/wagering/transactions/" + operacao.TransactionID,
		"pela identidade externa": "/providers/provider-a/wagering/transactions/tx-do-a",
	} {
		t.Run(nome, func(t *testing.T) {
			resp, corpo := a.chamarComVerificador(t, http.MethodGet, caminho, "", outro, true, nil)

			// 404 e não 403: um 403 confirmaria que a transação existe, e a
			// existência já é informação — diz que o provedor A operou aquele
			// identificador.
			assert.Equal(t, http.StatusNotFound, resp.StatusCode)
			corpoTexto := string(corpo)
			for _, vazamento := range []string{carteira.ID, jogador.String(), "tx-do-a", "30.00", "provider-a"} {
				assert.NotContains(t, corpoTexto, vazamento,
					"a resposta não pode carregar dado da transação alheia")
			}
		})
	}
}
