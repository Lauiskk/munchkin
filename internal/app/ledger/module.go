package ledger

import "go.uber.org/fx"

// Module provê os casos de uso da contabilidade de partidas dobradas.
var Module = fx.Module("app.ledger",
	fx.Provide(NewTrialBalancer),
)
