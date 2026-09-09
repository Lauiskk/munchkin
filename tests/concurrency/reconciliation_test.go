//go:build integration

package concurrency_test

import (
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/internal/adapter/postgres"
	"github.com/Lauiskk/munchkin/internal/app"
	appwallet "github.com/Lauiskk/munchkin/internal/app/wallet"
	domain "github.com/Lauiskk/munchkin/internal/domain/wagering"
)

// AC-12 — a reconciliação não pode acusar divergência que não existe.
//
// Ler o saldo e somar o ledger em duas consultas separadas compararia o estado
// de dois instantes: uma aposta que commitasse entre elas apareceria numa e não
// na outra. O sistema estaria correto e o alarme dispararia mesmo assim — e um
// alarme que dispara sozinho é pior que alarme nenhum, porque ensina quem o
// atende a ignorá-lo. É a única instrução única que garante o contrário.
func TestReconciliacaoNaoAcusaDivergenciaSobMovimentacao(t *testing.T) {
	const (
		apostas      = 60
		conferencias = 60
	)

	c := novoCenario(t)
	w, p := c.abrirCarteira(t, "1000.00")

	reconciler := appwallet.NewReconciler(postgres.NewWalletRepository(c.db), app.NopMetrics{},
		slog.New(slog.NewJSONHandler(io.Discard, nil)))

	largada := make(chan struct{})
	var wg sync.WaitGroup

	// Um lado move dinheiro sem parar.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-largada
		for i := range apostas {
			out, err := c.processor.Process(c.ctx,
				aposta(t, w, p, fmt.Sprintf("tx-%d", i), "1.00"))
			if err != nil || out.Status != domain.Processed {
				return
			}
		}
	}()

	// O outro confere sem parar, no meio da movimentação.
	divergentes := make([]appwallet.Reconciliation, 0, conferencias)
	var mu sync.Mutex

	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-largada
			for range conferencias / 4 {
				r, err := reconciler.Run(c.ctx, w)
				if err != nil {
					return
				}
				if !r.Consistent {
					mu.Lock()
					divergentes = append(divergentes, r)
					mu.Unlock()
				}
			}
		}()
	}

	close(largada)
	wg.Wait()

	require.Empty(t, divergentes,
		"toda conferência tem de ver saldo e ledger do MESMO instante; divergências vistas: %v",
		divergentes)

	// E, no fim, a conta continua fechando de verdade.
	final, err := reconciler.Run(c.ctx, w)
	require.NoError(t, err)
	assert.True(t, final.Consistent)
	assert.Equal(t, "0.00 BRL", final.Difference.String())
}
