// Package app is the use-case catalog of the auction context: one
// Application{Commands, Queries} value is assembled in service/ and
// injected into every port; ports call handlers only — never adapters
// or repositories directly (rule 28). Every handler arrives wrapped in
// the common decorators (logging, RED metrics, tracing — rule 29).
package app

import (
	"molot/internal/auction/app/command"
	"molot/internal/auction/app/query"
	"molot/internal/common/decorator"
)

type Application struct {
	Commands Commands
	Queries  Queries
}

// Commands — 8 write use cases (§3).
type Commands struct {
	ListAuction   decorator.CommandHandler[command.ListAuction]
	PlaceBid      decorator.CommandHandler[command.PlaceBid]
	CancelAuction decorator.CommandHandler[command.CancelAuction]
	CloseAuction  decorator.CommandHandler[command.CloseAuction]

	// Saga facade commands — called only through service.Facade.
	AwardToRunnerUp   decorator.CommandHandler[command.AwardToRunnerUp]
	RelistAuction     decorator.CommandHandler[command.RelistAuction]
	MarkSaleFailed    decorator.CommandHandler[command.MarkSaleFailed]
	ConfirmSettlement decorator.CommandHandler[command.ConfirmSettlement]
}

// Queries — 4 read use cases (§3).
type Queries struct {
	ActiveAuctionsCatalog decorator.QueryHandler[query.ActiveAuctionsCatalog, query.CatalogPage]
	AuctionCard           decorator.QueryHandler[query.AuctionCard, query.AuctionCardView]
	BidHistory            decorator.QueryHandler[query.BidHistory, query.BidHistoryPage]
	SellerDashboard       decorator.QueryHandler[query.SellerDashboard, query.DashboardView]
}
