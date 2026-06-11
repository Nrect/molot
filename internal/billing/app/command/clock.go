package command

import "time"

// clock is the consumer-side time source of the billing commands
// (BOOK_AUDIT rule 5): aggregates take `now` as a parameter, handlers
// obtain it here — unit tests stay deterministic without sleeps.
type clock interface {
	Now() time.Time
}
