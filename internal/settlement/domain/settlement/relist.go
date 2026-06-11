package settlement

import (
	"errors"
	"time"
)

// RelistPolicy is when and for how long the replacement listing runs —
// a snapshot of RELIST_DELAY / RELIST_DURATION taken once at service
// start (§6.3). The window calculation is a stateless domain function
// kept out of the handlers (rule 13).
type RelistPolicy struct {
	delay    time.Duration
	duration time.Duration
}

func NewRelistPolicy(delay, duration time.Duration) (RelistPolicy, error) {
	if delay <= 0 {
		return RelistPolicy{}, errors.New("relist delay must be positive")
	}
	if duration <= 0 {
		return RelistPolicy{}, errors.New("relist duration must be positive")
	}
	return RelistPolicy{delay: delay, duration: duration}, nil
}

// Window is the [startsAt, endsAt) bidding window of the replacement
// auction relative to now.
func (p RelistPolicy) Window(now time.Time) (startsAt, endsAt time.Time) {
	startsAt = now.UTC().Add(p.delay)
	return startsAt, startsAt.Add(p.duration)
}

func (p RelistPolicy) IsZero() bool { return p == RelistPolicy{} }
