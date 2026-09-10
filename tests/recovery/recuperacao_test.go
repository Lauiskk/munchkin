//go:build integration

package recovery_test

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/internal/adapter/postgres"
	"github.com/Lauiskk/munchkin/tests/infra"
)

// esperarPor bloqueia até a condição valer, ou falha dizendo o que esperava.
//
// Espera por CONDIÇÃO, nunca por `sleep` fixo torcendo para ter dado tempo: um
// sleep que basta na máquina de quem escreveu é o que faz suíte piscar na
// máquina de quem avalia.
func esperarPor(t *testing.T, oque string, prazo time.Duration, cond func() bool) {
	t.Helper()
	limite := time.Now().Add(prazo)
	for time.Now().Before(limite) {
		if cond() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("condição não alcançada em %v: %s", prazo, oque)
}

// esperarApertado sonda em intervalo curto, para alcançar uma janela estreita.
func esperarApertado(t *testing.T, oque string, prazo time.Duration, cond func() bool) {
	t.Helper()
	limite := time.Now().Add(prazo)
	for time.Now().Before(limite) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condição não alcançada em %v: %s", prazo, oque)
}

// AC-5 — §13.8: a pendência de uma instância morta é retomada por outra.
func TestPendenciaDeInstanciaMortaEhRetomadaPorOutra(t *testing.T) {
	a, db := tresInstanciasCom(t)
	carteira, jogador := a.abrirCarteira(t, "100.00")

	referencia := externo(t, "bet")
	reversao := externo(t, "rf")

	// A reversão chega ANTES da aposta: fica pendente na primeira instância.
	corpo := fmt.Sprintf(`{"providerId":"provider-a","externalTransactionId":%q,
	  "playerId":%q,"walletId":%q,"roundId":"round-1","gameId":"g",
	  "kind":"REFUND","money":{"amount":"40.00","currency":"BRL"},
	  "referenceExternalTransactionId":%q}`, reversao, jogador, carteira, referencia)
	status, resposta := chamar(t, a.instancias[0], http.MethodPost, "/wagering/transactions",
		a.provedor, corpo, map[string]string{"Idempotency-Key": "provider-a:" + reversao})
	require.Equal(t, http.StatusAccepted, status, string(resposta))

	// A instância que criou a pendência morre sem avisar ninguém.
	a.instancias[0].matar()

	// A aposta referenciada chega por OUTRA instância.
	status, resposta = a.aposta(t, a.instancias[1], referencia, carteira, jogador, "40.00")
	require.Equal(t, http.StatusOK, status, string(resposta))

	esperarPor(t, "a pendência ser resolvida por outra instância", 60*time.Second, func() bool {
		return escalar(t, db, `SELECT count(*) FROM wager_transactions
		         WHERE external_transaction_id = ? AND status = 'PROCESSED'`, reversao) == 1
	})

	assert.Equal(t, int64(10000),
		escalar(t, db, `SELECT balance_minor FROM wallets WHERE id = ?::uuid`, carteira),
		"debitou 40.00 na aposta e devolveu 40.00 no estorno")
	conferirLedger(t, db, carteira)
}

// AC-6, AC-7, AC-9 — §13.8: um `kill -9` no meio do trabalho não deixa rastro.
func TestInstanciaMortaNoMeioDoTrabalhoNaoDeixaDinheiroInconsistente(t *testing.T) {
	const envios = 120

	a, db := tresInstanciasCom(t)
	carteira, jogador := a.abrirCarteira(t, "1000.00")

	aceitas := make([]string, 0, envios)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for i := range envios {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := externo(t, fmt.Sprintf("tx-%d", i))
			// Sempre na PRIMEIRA instância: é ela que vai morrer.
			status, _ := a.aposta(t, a.instancias[0], id, carteira, jogador, "1.00")
			if status == http.StatusOK {
				mu.Lock()
				aceitas = append(aceitas, id)
				mu.Unlock()
			}
		}()
	}

	// Mata no meio: algumas respostas já saíram, outras nunca sairão.
	//
	// A sondagem é apertada de propósito. Com intervalo de centenas de
	// milissegundos, as apostas todas terminavam ANTES do kill e o teste passava
	// sem exercitar recuperação nenhuma — descoberto exigindo que a morte tenha
	// interrompido algo, e vendo o teste ficar vermelho três vezes seguidas.
	esperarApertado(t, "as primeiras apostas terem sido processadas", 30*time.Second, func() bool {
		return escalar(t, db, `SELECT count(*) FROM wallet_ledger_entries
		         WHERE wallet_id = ?::uuid AND direction = 'DEBIT'`, carteira) >= 2
	})
	a.instancias[0].matar()
	wg.Wait()

	// A invariante central: o que foi confirmado ao cliente está no ledger, e o
	// saldo bate. Quantas passaram não importa — matar no meio é justamente não
	// controlar isso.
	conferirLedger(t, db, carteira)

	for _, id := range aceitas {
		assert.Equal(t, int64(1),
			escalar(t, db, `SELECT count(*) FROM wager_transactions
			         WHERE external_transaction_id = ? AND status = 'PROCESSED'`, id),
			"o que foi respondido como processado tem de estar processado: %s", id)
	}
	require.NotEmpty(t, aceitas, "o cenário só vale se alguma aposta tiver sido aceita")
	require.Less(t, len(aceitas), envios,
		"o cenário só vale se a morte tiver INTERROMPIDO alguma coisa; com todas "+
			"aceitas, ele passaria sem exercitar recuperação nenhuma")

	// AC-7: a idempotência sobreviveu à morte do processo que a registrou.
	status, resposta := a.aposta(t, a.instancias[1], aceitas[0], carteira, jogador, "1.00")
	require.Equal(t, http.StatusOK, status, string(resposta))
	assert.Contains(t, string(resposta), `"idempotentReplay":true`,
		"o reenvio por outra instância tem de encontrar o resultado persistido")

	// Pode haver MAIS débitos que respostas recebidas, e isso não é defeito: a
	// transação commita antes de a resposta sair, então o processo pode morrer
	// no meio e o cliente nunca saber que a aposta passou. É exatamente para
	// isso que existe a idempotência — o reenvio acima encontra o resultado.
	//
	// O que não pode haver é débito a MAIS que operação processada: aí sim
	// seria dinheiro movido duas vezes.
	debitos := escalar(t, db, `SELECT count(*) FROM wallet_ledger_entries
	         WHERE wallet_id = ?::uuid AND direction = 'DEBIT'`, carteira)
	processadas := escalar(t, db, `SELECT count(*) FROM wager_transactions
	         WHERE wallet_id = ?::uuid AND provider_id IS NOT NULL AND status = 'PROCESSED'`,
		carteira)

	assert.GreaterOrEqual(t, debitos, int64(len(aceitas)),
		"tudo que foi confirmado ao cliente tem de estar no ledger")
	assert.Equal(t, processadas, debitos,
		"um lançamento por operação processada: nem duplicado, nem faltando")
}

// AC-8, AC-E3 — §13.5: reentrega depois do commit não produz segundo efeito.
//
// A reentrega é provocada reenviando a MESMA mensagem, e não cronometrando um
// `kill` entre o commit e a remoção. Os dois produzem exatamente a mesma
// situação para o consumidor — uma mensagem que já foi tratada chegando de
// novo — e a diferença é que este é determinístico. Cronometrar uma janela de
// microssegundos daria um teste que passa por sorte.
func TestReentregaDepoisDoCommitNaoDuplicaEfeito(t *testing.T) {
	a, db := tresInstanciasCom(t)
	carteira, jogador := a.abrirCarteira(t, "100.00")

	id := externo(t, "msg")
	mensagem := fmt.Sprintf(`{"messageId":%q,"type":"WagerTransactionRequested",
	  "occurredAt":"2026-09-09T12:00:00.000Z",
	  "data":{"providerId":"provider-a","externalTransactionId":%q,
	          "idempotencyKey":"provider-a:%s","playerId":%q,"walletId":%q,
	          "roundId":"round-1","gameId":"g","kind":"BET",
	          "money":{"amount":"30.00","currency":"BRL"}}}`,
		id, id, id, jogador, carteira)

	a.pilha.Publicar(t, id+"-1", mensagem)
	esperarPor(t, "a mensagem ser tratada", 60*time.Second, func() bool {
		return escalar(t, db, `SELECT count(*) FROM inbox_messages
		         WHERE message_id = ? AND completed_at IS NOT NULL`, id) == 1
	})
	require.Equal(t, int64(7000),
		escalar(t, db, `SELECT balance_minor FROM wallets WHERE id = ?::uuid`, carteira))

	// A mesma mensagem chega de novo, três vezes.
	for n := range 3 {
		a.pilha.Publicar(t, fmt.Sprintf("%s-re-%d", id, n), mensagem)
	}

	esperarPor(t, "a fila esvaziar", 60*time.Second, func() bool {
		return a.pilha.MensagensNaFila(t) == 0
	})

	assert.Equal(t, int64(7000),
		escalar(t, db, `SELECT balance_minor FROM wallets WHERE id = ?::uuid`, carteira),
		"três reentregas depois do commit não podem debitar de novo")
	assert.Equal(t, int64(1),
		escalar(t, db, `SELECT count(*) FROM wallet_ledger_entries
		         WHERE wallet_id = ?::uuid AND direction = 'DEBIT'`, carteira),
		"um único lançamento")
	assert.Equal(t, int64(1),
		escalar(t, db, `SELECT count(*) FROM inbox_messages WHERE message_id = ?`, id),
		"uma única linha de inbox: a identidade é a mesma")
	conferirLedger(t, db, carteira)
}

// tresInstanciasCom devolve o ambiente já com a pilha acessível.
func tresInstanciasCom(t *testing.T) (*ambiente, *postgres.Database) {
	t.Helper()
	pilha := infra.Start(t)
	db := pilha.Conectar(t)

	a := &ambiente{t: t, pilha: pilha}
	env := pilha.Env()
	for _, nome := range []string{"alfa", "bravo", "charlie"} {
		a.subir(t, nome, env)
	}
	a.admin = token(t, pilha.KeycloakURL, clientAdmin, secretAdmin)
	a.provedor = token(t, pilha.KeycloakURL, clientProviderA, secretProviderA)
	return a, db
}
