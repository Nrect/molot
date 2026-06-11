package auction

import "time"

// BiddingWindow is the time interval during which bids are accepted.
// originalEndsAt keeps the pre-anti-snipe deadline for transparency.
type BiddingWindow struct {
	startsAt       time.Time
	endsAt         time.Time
	originalEndsAt time.Time
}

// NewBiddingWindow validates startsAt < endsAt; originalEndsAt starts
// equal to endsAt and is preserved across extensions.
func NewBiddingWindow(startsAt, endsAt time.Time) (BiddingWindow, error) {
	if startsAt.IsZero() || endsAt.IsZero() || !endsAt.After(startsAt) {
		return BiddingWindow{}, ErrInvalidBiddingWindow
	}
	return BiddingWindow{startsAt: startsAt, endsAt: endsAt, originalEndsAt: endsAt}, nil
}

// UnmarshalBiddingWindow rehydrates a window from storage, where the
// current deadline may already have been extended past the original.
func UnmarshalBiddingWindow(startsAt, endsAt, originalEndsAt time.Time) (BiddingWindow, error) {
	if startsAt.IsZero() || endsAt.IsZero() || originalEndsAt.IsZero() {
		return BiddingWindow{}, ErrInvalidBiddingWindow
	}
	if !endsAt.After(startsAt) || endsAt.Before(originalEndsAt) {
		return BiddingWindow{}, ErrInvalidBiddingWindow
	}
	return BiddingWindow{startsAt: startsAt, endsAt: endsAt, originalEndsAt: originalEndsAt}, nil
}

func (w BiddingWindow) IsZero() bool              { return w == BiddingWindow{} }
func (w BiddingWindow) StartsAt() time.Time       { return w.startsAt }
func (w BiddingWindow) EndsAt() time.Time         { return w.endsAt }
func (w BiddingWindow) OriginalEndsAt() time.Time { return w.originalEndsAt }

// IsOpenAt reports whether now falls inside [startsAt, endsAt).
func (w BiddingWindow) IsOpenAt(now time.Time) bool {
	return !now.Before(w.startsAt) && now.Before(w.endsAt)
}

// ExtendedBy returns a window with the deadline pushed by d; the
// original deadline is kept for the record.
func (w BiddingWindow) ExtendedBy(d time.Duration) BiddingWindow {
	return BiddingWindow{
		startsAt:       w.startsAt,
		endsAt:         w.endsAt.Add(d),
		originalEndsAt: w.originalEndsAt,
	}
}
