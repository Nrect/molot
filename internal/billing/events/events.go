// Package events defines the billing context's public integration
// events — the ONLY billing package other contexts may import.
//
// Events are flat, versioned and append-only: a breaking change means a
// new V2 struct dual-published next to V1, never a field edit. Payloads
// carry primitives and time.Time only; all timestamps are UTC.
//
// InvoiceVoided deliberately has no integration event: void is invoked
// synchronously by the settlement saga and has no other consumers.
package events

import "time"

// Topic is the watermill topic billing integration events are published to.
const Topic = "billing-events"

type InvoiceIssuedV1 struct {
	EventID         string    `json:"event_id"`
	InvoiceID       string    `json:"invoice_id"`
	AuctionID       string    `json:"auction_id"`
	DebtorID        string    `json:"debtor_id"`
	HammerMinor     int64     `json:"hammer_minor"`
	CommissionMinor int64     `json:"commission_minor"`
	TotalMinor      int64     `json:"total_minor"`
	Currency        string    `json:"currency"`
	DueAt           time.Time `json:"due_at"`
	Attempt         int       `json:"attempt"` // 1 | 2 (second chance)
	OccurredAt      time.Time `json:"occurred_at"`
}

type InvoicePaidV1 struct {
	EventID    string    `json:"event_id"`
	InvoiceID  string    `json:"invoice_id"`
	AuctionID  string    `json:"auction_id"`
	DebtorID   string    `json:"debtor_id"`
	TotalMinor int64     `json:"total_minor"`
	Currency   string    `json:"currency"`
	OccurredAt time.Time `json:"occurred_at"`
}

type InvoiceExpiredV1 struct {
	EventID    string    `json:"event_id"`
	InvoiceID  string    `json:"invoice_id"`
	AuctionID  string    `json:"auction_id"`
	DebtorID   string    `json:"debtor_id"`
	Attempt    int       `json:"attempt"` // 1 | 2 (second chance)
	OccurredAt time.Time `json:"occurred_at"`
}
