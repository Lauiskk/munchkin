// Package safe protege goroutines contra pânico.
//
// Um pânico dentro de uma goroutine NÃO é alcançado pelo recover de quem a
// iniciou: ele derruba o processo inteiro. O recover do servidor HTTP cobre os
// tratadores de requisição e nada além disso — qualquer worker, consumidor ou
// tarefa de fundo precisa da sua própria proteção, e é o que este pacote dá.
//
// A escolha é deliberadamente conservadora: um pânico é registrado como falha
// grave e a goroutine termina. Não há reinício automático aqui, porque um
// laço que entra em pânico a cada iteração viraria um laço de reinício
// consumindo CPU e enchendo o log. Quem quiser retomar o trabalho declara isso
// explicitamente com Loop.
package safe

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/Lauiskk/munchkin/pkg/logs"
)

// Go executa fn numa goroutine, convertendo pânico em log de erro.
//
// O nome identifica a tarefa no log; sem ele um pânico vira uma pilha órfã que
// ninguém consegue associar a nada.
func Go(ctx context.Context, log *slog.Logger, name string, fn func()) {
	go func() {
		defer Recover(ctx, log, name)
		fn()
	}()
}

// Recover é o defer que converte pânico em log. Use diretamente quando a
// goroutine já existe e só falta protegê-la.
func Recover(ctx context.Context, log *slog.Logger, name string) {
	r := recover()
	if r == nil {
		return
	}
	log.LogAttrs(ctx, slog.LevelError, "panic.recovered",
		slog.String("task", name),
		slog.String(logs.KeyError, fmt.Sprint(r)),
		slog.String("stack", string(debug.Stack())),
	)
}

// Loop executa fn repetidamente até o context ser cancelado, sobrevivendo a
// pânico em qualquer iteração.
//
// Existe para os workers: a queda de uma iteração não pode encerrar o worker,
// senão uma mensagem malformada tiraria o consumidor do ar até o próximo
// reinício. O intervalo entre iterações após um pânico impede que uma falha
// determinística vire laço quente.
func Loop(ctx context.Context, log *slog.Logger, name string, cooldown time.Duration, fn func(context.Context) error) {
	for {
		if ctx.Err() != nil {
			return
		}
		if panicked := runOnce(ctx, log, name, fn); panicked {
			select {
			case <-ctx.Done():
				return
			case <-time.After(cooldown):
			}
		}
	}
}

// runOnce executa uma iteração e informa se ela entrou em pânico.
func runOnce(ctx context.Context, log *slog.Logger, name string, fn func(context.Context) error) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			log.LogAttrs(ctx, slog.LevelError, "panic.recovered",
				slog.String("task", name),
				slog.String(logs.KeyError, fmt.Sprint(r)),
				slog.String("stack", string(debug.Stack())),
			)
		}
	}()
	if err := fn(ctx); err != nil && ctx.Err() == nil {
		log.LogAttrs(ctx, slog.LevelError, "task.failed",
			slog.String("task", name),
			slog.String(logs.KeyError, err.Error()))
	}
	return false
}
