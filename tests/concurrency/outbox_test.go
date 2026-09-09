//go:build integration

package concurrency_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/internal/adapter/postgres"
	"github.com/Lauiskk/munchkin/internal/app/outbox"
)

// coletor registra o que cada publicador enviou, contando por evento.
type coletor struct {
	mu     sync.Mutex
	porID  map[string]int
	total  int
	atraso time.Duration
}

func novoColetor(atraso time.Duration) *coletor {
	return &coletor{porID: map[string]int{}, atraso: atraso}
}

func (c *coletor) Publish(_ context.Context, m outbox.Message) error {
	// O atraso é o que dá à disputa tempo de acontecer. Sem ele, um publicador
	// terminaria a fila antes de o outro chegar, e o teste passaria sem nunca
	// exercitar a coordenação.
	time.Sleep(c.atraso)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.porID[m.DeduplicationID]++
	c.total++
	return nil
}

func (c *coletor) duplicados() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	repetidos := map[string]int{}
	for id, n := range c.porID {
		if n > 1 {
			repetidos[id] = n
		}
	}
	return repetidos
}

// AC-6 — dois publicadores sobre a mesma outbox.
//
// Publicar o mesmo evento duas vezes não corrompe saldo, e a fila FIFO ainda
// deduplicaria pelo eventId. Mas at-least-once é o piso, não a meta: um
// publicador que reenvia porque não sabe coordenar gasta o orçamento de
// deduplicação da fila (cinco minutos) com repetição evitável, e esconde a
// falha de coordenação até o dia em que a janela não bastar.
//
// O que este teste cobra é o FILTRO DE LEASE, não o SKIP LOCKED. Verificado por
// mutação: sem o filtro, cada evento sai duas vezes e o teste fica vermelho;
// sem o SKIP LOCKED, ele continua verde, porque quem perde apenas espera o lock
// e depois não encontra nada elegível.
func TestDoisPublicadoresNaoEnviamOMesmoEventoDuasVezes(t *testing.T) {
	const (
		carteiras    = 15
		concorrentes = 4
	)

	c := novoCenario(t)
	for range carteiras {
		c.abrirCarteira(t, "100.00") // dois eventos por abertura
	}

	esperados := int(c.escalar(t,
		`SELECT count(*) FROM outbox_events WHERE published_at IS NULL`))
	require.Equal(t, carteiras*2, esperados)

	envio := novoColetor(5 * time.Millisecond)
	var diario registro

	publicadores := make([]*outbox.Dispatcher, concorrentes)
	for i := range publicadores {
		publicadores[i] = outbox.NewDispatcher(
			postgres.NewOutboxRepository(c.db), envio,
			slog.New(slog.NewJSONHandler(&diario, nil)),
			"instancia-"+string(rune('a'+i)), 50, 30*time.Second)
	}

	largada := make(chan struct{})
	publicados := make([]int, concorrentes)
	erros := make([]error, concorrentes)
	var wg sync.WaitGroup
	for i, p := range publicadores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-largada
			publicados[i], erros[i] = p.RunOnce(c.ctx)
		}()
	}
	close(largada)
	wg.Wait()

	soma := 0
	for i := range concorrentes {
		require.NoError(t, erros[i])
		soma += publicados[i]
	}

	assert.Equal(t, esperados, soma,
		"cada evento publicado exatamente uma vez pelo conjunto dos publicadores")
	assert.Empty(t, envio.duplicados(),
		"nenhum evento pode ir para o transporte duas vezes na mesma rodada")
	assert.NotContains(t, diario.String(), "outbox.publish_failed",
		"perder a disputa é rotina e sai limpo; erro aqui é publicador pisando no outro")
	assert.Zero(t, c.escalar(t, `SELECT count(*) FROM outbox_events WHERE published_at IS NULL`),
		"a outbox tem de ficar vazia")
	assert.Zero(t, c.escalar(t, `SELECT count(*) FROM outbox_events WHERE locked_by IS NOT NULL`),
		"nenhum lease pode ficar pendurado")
}

// registro acumula o log dos publicadores com exclusão mútua: vários escrevem
// ao mesmo tempo, e sem o mutex o -race apontaria para o teste.
type registro struct {
	mu    sync.Mutex
	texto strings.Builder
}

func (r *registro) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.texto.Write(p)
}

func (r *registro) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.texto.String()
}

var _ io.Writer = (*registro)(nil)
