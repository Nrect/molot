package app

import (
	"molot/internal/common/decorator"
	"molot/internal/settlement/app/command"
	"molot/internal/settlement/app/query"
)

// Application is the settlement context's use-case catalog (rule 28):
// ports receive it and call ONLY these handlers — never adapters or the
// repository directly. Handlers are wrapped with the common decorators
// (logging, RED metrics, tracing) by the service package.
type Application struct {
	Commands Commands
	Queries  Queries
}

type Commands struct {
	DeclineSecondChanceOffer decorator.CommandHandler[command.DeclineSecondChanceOffer]
}

type Queries struct {
	SettlementStatus decorator.QueryHandler[query.SettlementStatus, query.SettlementView]
}
