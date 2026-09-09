package safe_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lauiskk/munchkin/pkg/safe"
)

func newTestLogger() (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// syncBuffer permite ler o log de outra goroutine sem corrida.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Um pânico em goroutine derruba o processo inteiro se ninguém o recuperar.
// Este teste falharia com o processo morrendo, não com uma asserção — que é
// exatamente o comportamento que o pacote existe para impedir.
func TestGoRecuperaPanicoEmGoroutine(t *testing.T) {
	log, buf := newTestLogger()
	done := make(chan struct{})

	safe.Go(context.Background(), log, "tarefa-de-teste", func() {
		defer close(done)
		panic("estouro proposital")
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a goroutine não executou")
	}
	// O log é escrito no defer, depois de close(done).
	require.Eventually(t, func() bool {
		return buf.String() != ""
	}, 2*time.Second, 10*time.Millisecond)

	var rec map[string]any
	require.NoError(t, json.Unmarshal([]byte(firstLine(buf.String())), &rec))
	assert.Equal(t, "panic.recovered", rec["msg"])
	assert.Equal(t, "tarefa-de-teste", rec["task"])
	assert.Contains(t, rec["error"], "estouro proposital")
	assert.Contains(t, rec["stack"], "safe_test", "a pilha precisa apontar para a origem")
}

// Uma iteração que entra em pânico não pode encerrar o worker: bastaria uma
// mensagem malformada para tirar o consumidor do ar até o próximo reinício.
func TestLoopSobreviveAPanicoEContinua(t *testing.T) {
	log, _ := newTestLogger()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var iteracoes atomic.Int32
	go safe.Loop(ctx, log, "worker", time.Millisecond, func(context.Context) error {
		n := iteracoes.Add(1)
		if n == 1 {
			panic("primeira iteração explode")
		}
		if n >= 3 {
			cancel()
		}
		return nil
	})

	require.Eventually(t, func() bool { return iteracoes.Load() >= 3 },
		3*time.Second, 10*time.Millisecond,
		"o laço deveria ter continuado depois do pânico")
}

func TestLoopEncerraQuandoOContextEhCancelado(t *testing.T) {
	log, _ := newTestLogger()
	ctx, cancel := context.WithCancel(context.Background())

	parou := make(chan struct{})
	go func() {
		defer close(parou)
		safe.Loop(ctx, log, "worker", time.Millisecond, func(context.Context) error {
			return nil
		})
	}()

	cancel()
	select {
	case <-parou:
	case <-time.After(2 * time.Second):
		t.Fatal("o laço ignorou o cancelamento do context")
	}
}

// Erro devolvido pela iteração é registrado, mas não derruba o laço.
func TestLoopRegistraErroSemEncerrar(t *testing.T) {
	log, buf := newTestLogger()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var n atomic.Int32
	go safe.Loop(ctx, log, "worker", time.Millisecond, func(context.Context) error {
		if n.Add(1) >= 2 {
			cancel()
		}
		return errors.New("falha transitória")
	})

	require.Eventually(t, func() bool { return n.Load() >= 2 }, 3*time.Second, 10*time.Millisecond)
	assert.Contains(t, buf.String(), "task.failed")
	assert.Contains(t, buf.String(), "falha transitória")
}

func firstLine(s string) string {
	for i, r := range s {
		if r == '\n' {
			return s[:i]
		}
	}
	return s
}
