// Package app is the application layer of the billing context: the
// single catalog of its use cases (BOOK_AUDIT rule 28). Ports receive
// Application and call ONLY these handlers — never adapters or the
// repository directly. Every handler is wrapped with the common
// decorators (logging, RED metrics, tracing) by the service package.
package app

import (
	"molot/internal/billing/app/command"
	"molot/internal/billing/app/query"
	"molot/internal/common/decorator"
)

type Application struct {
	Commands Commands
	Queries  Queries
}

type Commands struct {
	IssueInvoice  decorator.CommandHandler[command.IssueInvoice]
	PayInvoice    decorator.CommandHandler[command.PayInvoice]
	ExpireInvoice decorator.CommandHandler[command.ExpireInvoice]
	VoidInvoice   decorator.CommandHandler[command.VoidInvoice]
}

type Queries struct {
	InvoiceByID             decorator.QueryHandler[query.InvoiceByID, query.InvoiceView]
	PendingInvoicesOfBidder decorator.QueryHandler[query.PendingInvoicesOfBidder, []query.InvoiceView]
}
