package wallet

import (
	"go.uber.org/fx"

	"github.com/Lauiskk/munchkin/internal/app"
)

// Module provê os casos de uso de carteira.
//
// Cada pacote de caso de uso declara o próprio módulo, em vez de um módulo
// central listar todos. Um módulo central importaria cada caso de uso, e cada
// caso de uso importa o pacote comum — o que fecha um ciclo. Além disso,
// acrescentar um caso de uso passa a não exigir editar um arquivo distante.
var Module = fx.Module("app.wallet",
	fx.Provide(
		NewOpener,
		NewGetter,
	),
)

// ClockModule provê o relógio compartilhado pelos casos de uso.
//
// Fica separado porque é comum a todos: declará-lo em cada módulo faria o Fx
// recusar a composição por construtor duplicado.
var ClockModule = fx.Module("app.clock",
	fx.Provide(func() app.Clock { return app.SystemClock{} }),
)
