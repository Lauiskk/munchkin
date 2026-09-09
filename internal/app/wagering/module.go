package wagering

import "go.uber.org/fx"

// Module provê os casos de uso de operação financeira.
var Module = fx.Module("app.wagering",
	fx.Provide(
		NewProcessor,
		NewQuerier,
		NewResolver,
	),
)
