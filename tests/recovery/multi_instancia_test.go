//go:build integration

package recovery_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/internal/adapter/postgres"
)

// abrirCarteira cria uma carteira pela primeira instância.
func (a *ambiente) abrirCarteira(t *testing.T, saldo string) (string, string) {
	t.Helper()
	jogador := uuid.NewString()

	status, corpo := chamar(t, a.instancias[0], http.MethodPost, "/wallets", a.admin,
		fmt.Sprintf(`{"playerId":%q,"initialBalance":{"amount":%q,"currency":"BRL"}}`,
			jogador, saldo), nil)
	require.Equal(t, http.StatusCreated, status, string(corpo))

	var w struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(corpo, &w))
	return w.ID, jogador
}

// aposta envia uma operação para a instância indicada.
func (a *ambiente) aposta(t *testing.T, inst *instancia, externo, carteira, jogador, valor string) (int, []byte) {
	t.Helper()
	corpo := fmt.Sprintf(`{"providerId":"provider-a","externalTransactionId":%q,
	  "playerId":%q,"walletId":%q,"roundId":"round-1","gameId":"fortune-chimp",
	  "kind":"BET","money":{"amount":%q,"currency":"BRL"}}`,
		externo, jogador, carteira, valor)
	return chamar(t, inst, http.MethodPost, "/wagering/transactions", a.provedor, corpo,
		map[string]string{"Idempotency-Key": "provider-a:" + externo})
}

// escalar lê um número do banco.
func escalar(t *testing.T, db *postgres.Database, q string, args ...any) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.Session(t.Context()).Raw(q, args...).Scan(&n).Error)
	return n
}

// conferirLedger é a checagem final que o §13 exige de todo cenário.
func conferirLedger(t *testing.T, db *postgres.Database, carteira string) {
	t.Helper()
	saldo := escalar(t, db, `SELECT balance_minor FROM wallets WHERE id = ?::uuid`, carteira)
	soma := escalar(t, db, `SELECT COALESCE(SUM(CASE WHEN direction = 'CREDIT'
	                          THEN amount_minor ELSE -amount_minor END), 0)
	                          FROM wallet_ledger_entries WHERE wallet_id = ?::uuid`, carteira)
	assert.Equal(t, saldo, soma,
		"saldo armazenado tem de bater com créditos menos débitos do ledger")
}

// AC-1, AC-2, AC-9 — §13.1 e §13.4 juntos.
//
// Cinquenta envios da mesma aposta, distribuídos entre TRÊS PROCESSOS. Dentro de
// um processo, um mutex resolveria — e é justamente por isso que este cenário
// existe: com processos separados não há memória compartilhada, e a única coisa
// que pode decidir é o banco.
func TestCinquentaEnviosEntreTresInstanciasProduzemUmDebito(t *testing.T) {
	const envios = 50

	a, db := tresInstanciasCom(t)
	carteira, jogador := a.abrirCarteira(t, "1000.00")

	status := make([]int, envios)
	var wg sync.WaitGroup
	largada := make(chan struct{})

	for i := range envios {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-largada
			inst := a.instancias[i%len(a.instancias)]
			status[i], _ = a.aposta(t, inst, externo(t, "tx-1"), carteira, jogador, "80.00")
		}()
	}
	close(largada)
	wg.Wait()

	aceitos := 0
	for _, s := range status {
		if s == http.StatusOK {
			aceitos++
		}
	}
	assert.Equal(t, envios, aceitos, "todo envio recebe resposta: um processa, os demais são replay")

	assert.Equal(t, int64(1),
		escalar(t, db, `SELECT count(*) FROM wallet_ledger_entries
		                 WHERE wallet_id = ?::uuid AND direction = 'DEBIT'`, carteira),
		"um único débito, com cinquenta envios entre três processos")
	assert.Equal(t, int64(92000),
		escalar(t, db, `SELECT balance_minor FROM wallets WHERE id = ?::uuid`, carteira),
		"1000.00 menos uma aposta de 80.00")
	conferirLedger(t, db, carteira)
}

// AC-3, AC-9 — §13.2 entre instâncias.
//
// O cenário que o §8 usa para avaliar, agora com os dois envios saindo de
// processos diferentes: o lock de linha é a única coisa entre eles.
func TestDisputaDeOitentaSobreCemEntreInstanciasDistintas(t *testing.T) {
	a, db := tresInstanciasCom(t)
	carteira, jogador := a.abrirCarteira(t, "100.00")

	tipos := make([]int, 2)
	corpos := make([][]byte, 2)
	var wg sync.WaitGroup
	largada := make(chan struct{})

	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-largada
			tipos[i], corpos[i] = a.aposta(t, a.instancias[i],
				externo(t, fmt.Sprintf("bet-%d", i)), carteira, jogador, "80.00")
		}()
	}
	close(largada)
	wg.Wait()

	processadas, recusadas := 0, 0
	for i := range 2 {
		switch tipos[i] {
		case http.StatusOK:
			processadas++
		case http.StatusUnprocessableEntity:
			recusadas++
			assert.Contains(t, string(corpos[i]), "INSUFFICIENT_FUNDS")
		default:
			t.Fatalf("status inesperado %d: %s", tipos[i], corpos[i])
		}
	}

	assert.Equal(t, 1, processadas, "exatamente uma aposta processada")
	assert.Equal(t, 1, recusadas, "exatamente uma recusada por saldo")
	assert.Equal(t, int64(2000),
		escalar(t, db, `SELECT balance_minor FROM wallets WHERE id = ?::uuid`, carteira),
		"saldo final de 20.00")
	conferirLedger(t, db, carteira)
}

// AC-4 — §13.3 entre instâncias.
func TestCarteirasDistintasProcessamEmParaleloEntreInstancias(t *testing.T) {
	const carteiras = 9

	a, db := tresInstanciasCom(t)

	ids := make([]string, carteiras)
	jogadores := make([]string, carteiras)
	for i := range carteiras {
		ids[i], jogadores[i] = a.abrirCarteira(t, "100.00")
	}

	var wg sync.WaitGroup
	largada := make(chan struct{})
	for i := range carteiras {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-largada
			status, corpo := a.aposta(t, a.instancias[i%len(a.instancias)],
				externo(t, fmt.Sprintf("tx-%d", i)), ids[i], jogadores[i], "40.00")
			assert.Equal(t, http.StatusOK, status, string(corpo))
		}()
	}
	close(largada)
	wg.Wait()

	for i := range carteiras {
		assert.Equal(t, int64(6000),
			escalar(t, db, `SELECT balance_minor FROM wallets WHERE id = ?::uuid`, ids[i]),
			"carteira %d: 100.00 menos 40.00, sem interferência das outras", i)
		conferirLedger(t, db, ids[i])
	}
}
