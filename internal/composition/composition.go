// Package composition declara o grafo de dependências da aplicação.
//
// A lista mora aqui, e não no `main`, para que o teste de composição verifique
// EXATAMENTE o grafo que roda. Enquanto ela estava duplicada, "os mesmos
// módulos do cmd/api" era uma promessa no comentário — e ela quebrou na primeira
// vez que um módulo novo entrou só de um lado.
package composition

import (
	"go.uber.org/fx"

	"github.com/Lauiskk/munchkin/internal/adapter/auth"
	"github.com/Lauiskk/munchkin/internal/adapter/http/server"
	"github.com/Lauiskk/munchkin/internal/adapter/ops"
	"github.com/Lauiskk/munchkin/internal/adapter/postgres"
	"github.com/Lauiskk/munchkin/internal/adapter/sqs"
	"github.com/Lauiskk/munchkin/internal/adapter/tracing"
	appinbox "github.com/Lauiskk/munchkin/internal/app/inbox"
	appwagering "github.com/Lauiskk/munchkin/internal/app/wagering"
	appwallet "github.com/Lauiskk/munchkin/internal/app/wallet"
	"github.com/Lauiskk/munchkin/internal/worker"
	"github.com/Lauiskk/munchkin/pkg/metrics"
)

// Aplicacao é o grafo completo: adaptadores, casos de uso e processos de fundo.
var Aplicacao = fx.Options(
	postgres.Module,
	appwallet.ClockModule,
	appwallet.Module,
	appinbox.Module,
	appwagering.Module,
	auth.Module,
	tracing.Module,
	metrics.Module,
	ops.Module,
	sqs.Module,
	server.Module,
	worker.Module,
)
