package auction

import "time"

// AntiSnipePolicy is the platform anti-sniping rule snapshotted onto
// the aggregate at listing time (§2.1): a bid landing within `window`
// of the deadline extends it by `extension`, at most `maxExtensions`
// times. Snapshotting keeps a running auction's rules deterministic.
type AntiSnipePolicy struct {
	window        time.Duration
	extension     time.Duration
	maxExtensions int
}

func NewAntiSnipePolicy(window, extension time.Duration, maxExtensions int) (AntiSnipePolicy, error) {
	if window <= 0 || extension <= 0 || maxExtensions < 0 {
		return AntiSnipePolicy{}, ErrInvalidAntiSnipePolicy
	}
	return AntiSnipePolicy{window: window, extension: extension, maxExtensions: maxExtensions}, nil
}

func (p AntiSnipePolicy) IsZero() bool             { return p == AntiSnipePolicy{} }
func (p AntiSnipePolicy) Window() time.Duration    { return p.window }
func (p AntiSnipePolicy) Extension() time.Duration { return p.extension }
func (p AntiSnipePolicy) MaxExtensions() int       { return p.maxExtensions }

// TriggersAt reports whether a bid placed at now is inside the snipe
// window of the endsAt deadline (now >= endsAt-window).
func (p AntiSnipePolicy) TriggersAt(now, endsAt time.Time) bool {
	return !now.Before(endsAt.Add(-p.window))
}
