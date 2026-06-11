// Package command holds the write use cases of the auction context:
// one file per use case, business language names, structs carrying
// domain types (rule 26). Handlers orchestrate only — every business
// `if` lives in the domain (rule 14). Domain sentinels are wrapped
// into transport-agnostic slug errors on this boundary (rule 30).
package command

import (
	"context"
	"errors"
	"time"

	"molot/internal/auction/domain/auction"
	"molot/internal/common/errs"
)

// clock is the consumer-side time source (§10): aggregates take `now`
// as a parameter, handlers obtain it here — deterministic tests, no
// sleeps.
type clock interface {
	Now() time.Time
}

// bidderProfiles reads the local projection of participant events.
// Contract: a bidder without a profile row is an unverified bidder —
// the projection is eventually consistent and absence must not block
// small bids (the verification guard lives in the domain).
type bidderProfiles interface {
	BidderByID(ctx context.Context, id auction.BidderID) (auction.Bidder, error)
}

// mapNotFound translates the domain NotFoundError into the
// ports-agnostic slug error; anything else passes through.
func mapNotFound(err error) (error, bool) {
	var notFound auction.NotFoundError
	if errors.As(err, &notFound) {
		return errs.NewNotFoundError("auction-not-found").WithCause(err), true
	}
	return err, false
}
