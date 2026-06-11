package settlement

import "context"

// Repository is the persistence contract of the Settlement saga state
// (interface in the domain package, rules 15-16).
//
// There is no acting-user parameter by design (rules 21-23
// documented): the saga is a system-driven process manager fed by
// integration events; its only user-facing command (decline) authorizes
// through the pure CanRunnerUpDecline in its decide phase — never
// through repository identity.
type Repository interface {
	// Add inserts the freshly started saga. Idempotent: a settlement
	// for the same auction already exists → silent no-op
	// (INSERT ... ON CONFLICT (auction_id) DO NOTHING, §6.1) — the
	// caller re-loads and continues from the stored state.
	Add(ctx context.Context, s *Settlement) error

	// Get loads the saga state without locking (the decide-phase read).
	// Missing → NotFoundError.
	Get(ctx context.Context, id AuctionID) (*Settlement, error)

	// Update runs updateFn on the saga under SELECT ... FOR UPDATE +
	// version (the commit phase, §6.1) and persists the returned value.
	// An updateFn error rolls everything back and is returned as-is —
	// ErrUnexpectedTransition stays recognizable for the caller's ack.
	Update(
		ctx context.Context,
		id AuctionID,
		updateFn func(ctx context.Context, s *Settlement) (*Settlement, error),
	) error
}
