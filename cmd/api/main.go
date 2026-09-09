// Command api é o ponto de entrada do serviço.
//
// A composição inteira vive aqui, declarada como módulos do Fx. Nenhum objeto é
// construído fora do grafo: quem precisa de uma dependência a recebe pelo
// construtor, e o Fx garante que ela existe antes e é fechada depois.
package main

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"

	"github.com/Lauiskk/munchkin/internal/adapter/auth"
	"github.com/Lauiskk/munchkin/internal/adapter/http/server"
	"github.com/Lauiskk/munchkin/internal/adapter/postgres"
	appwagering "github.com/Lauiskk/munchkin/internal/app/wagering"
	appwallet "github.com/Lauiskk/munchkin/internal/app/wallet"
	"github.com/Lauiskk/munchkin/internal/config"
	"github.com/Lauiskk/munchkin/pkg/logs"
)

// Prazos do ciclo de vida. A subida é curta porque tudo que ela faz é validar e
// conectar; o encerramento é mais longo porque precisa caber o dreno das
// requisições em andamento.
const (
	startTimeout = 30 * time.Second
	stopTimeout  = 45 * time.Second
)

func main() {
	// Subcomando de sonda, usado pelo HEALTHCHECK da imagem. Fica antes de
	// tudo porque não deve carregar configuração nem montar o grafo.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			os.Exit(runHealthCheck())
		case "migrate":
			os.Exit(runMigrate(os.Args[2:]))
		}
	}

	// A configuração é carregada fora do grafo para que um ambiente inválido
	// produza uma mensagem legível, e não um erro de construção de dependência
	// enterrado no relatório do Fx.
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	app := fx.New(
		fx.StartTimeout(startTimeout),
		fx.StopTimeout(stopTimeout),

		fx.Supply(cfg),
		fx.Provide(logs.New),

		// Os eventos do próprio Fx saem no mesmo log estruturado do resto, para
		// que a subida e o encerramento sejam legíveis pelas mesmas ferramentas.
		fx.WithLogger(func(log *slog.Logger) fxevent.Logger {
			return &fxevent.SlogLogger{Logger: log}
		}),

		postgres.Module,
		appwallet.ClockModule,
		appwallet.Module,
		appwagering.Module,
		auth.Module,
		server.Module,
	)

	app.Run()

	// Run devolve depois do encerramento. Um erro guardado aqui é falha na
	// subida ou na parada, e precisa virar código de saída — senão um container
	// que não conseguiu subir aparece como encerramento normal.
	if err := app.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "falha no ciclo de vida: %v\n", err)
		os.Exit(1)
	}
}
