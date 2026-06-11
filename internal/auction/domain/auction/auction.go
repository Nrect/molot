package auction

import (
	"errors"
	"time"
)

// Auction is the central aggregate of the context (§2.1). The full bid
// history belongs to its consistency boundary but is never loaded:
// bid invariants depend only on the head of state, so the aggregate
// keeps the top two bids denormalized (ADR-0003). All fields are
// unexported (rule 8); state changes go through behavior methods that
// guard the invariant and transition atomically (rule 9).
type Auction struct {
	id               AuctionID
	seller           SellerID
	lot              Lot
	startPrice       Money
	increment        Money
	reserve          ReservePrice // zero = no reserve; hidden from events/API
	window           BiddingWindow
	antiSnipe        AntiSnipePolicy // snapshot taken at listing time
	verifyAbove      Money           // snapshot of verified_bid_threshold at listing time
	extensionsUsed   int
	status           Status
	outcome          Outcome // zero until closed
	leadingBid       Bid     // zero value = no bids (no pointer flags)
	runnerUpBid      Bid
	winnerReassigned bool // "AwardToRunnerUp already done" marker for facade idempotency
	bidCount         int
	relistOf         AuctionID // zero = original listing
	relistGen        int       // 0 | 1 — cap of automatic relists
	settled          bool      // settlement finished (with success or failure)
	version          int64     // mapping only; the adapter increments it
	events           []DomainEvent
}

// New lists an auction, snapshotting the platform ListingRules onto
// the aggregate. Every blank/zero argument is rejected; a non-platform
// currency fails with ErrUnsupportedCurrency.
func New(
	id AuctionID,
	seller SellerID,
	lot Lot,
	startPrice, increment Money,
	reserve ReservePrice,
	window BiddingWindow,
	rules ListingRules,
	now time.Time,
) (*Auction, error) {
	if id.IsZero() || seller.IsZero() {
		return nil, ErrInvalidID
	}
	if lot.IsZero() {
		return nil, ErrInvalidLot
	}
	if rules.IsZero() {
		return nil, ErrInvalidListingRules
	}
	if now.IsZero() {
		return nil, errors.New("now must not be zero")
	}
	if startPrice.IsZero() || startPrice.AmountMinor() <= 0 {
		return nil, ErrInvalidStartPrice
	}
	if startPrice.Currency() != rules.Currency() {
		return nil, ErrUnsupportedCurrency
	}
	if increment.IsZero() || increment.AmountMinor() <= 0 {
		return nil, ErrInvalidIncrement
	}
	if increment.Currency() != startPrice.Currency() {
		return nil, ErrCurrencyMismatch
	}
	if !reserve.IsZero() && reserve.Money().Currency() != startPrice.Currency() {
		return nil, ErrCurrencyMismatch
	}
	if window.IsZero() {
		return nil, ErrInvalidBiddingWindow
	}
	if !window.EndsAt().After(now) {
		return nil, ErrInvalidBiddingWindow
	}

	a := &Auction{
		id:          id,
		seller:      seller,
		lot:         lot,
		startPrice:  startPrice,
		increment:   increment,
		reserve:     reserve,
		window:      window,
		antiSnipe:   rules.AntiSnipe(),
		verifyAbove: rules.VerifyAbove(),
		status:      StatusListed,
		version:     1,
	}
	a.record(AuctionListed{OccurredAt: now})
	return a, nil
}

// RelistedFrom builds the replacement auction for a failed sale: same
// lot, prices and rule snapshot, a fresh window, generation+1 (capped
// at 1 — ErrRelistLimitReached). The new aggregate records both
// AuctionListed and AuctionRelisted.
func RelistedFrom(orig *Auction, newID AuctionID, window BiddingWindow, now time.Time) (*Auction, error) {
	if orig == nil {
		return nil, errors.New("original auction must not be nil")
	}
	if newID.IsZero() {
		return nil, ErrInvalidID
	}
	if orig.relistGen >= 1 {
		return nil, ErrRelistLimitReached
	}
	if window.IsZero() || !window.EndsAt().After(now) {
		return nil, ErrInvalidBiddingWindow
	}

	a := &Auction{
		id:          newID,
		seller:      orig.seller,
		lot:         orig.lot,
		startPrice:  orig.startPrice,
		increment:   orig.increment,
		reserve:     orig.reserve,
		window:      window,
		antiSnipe:   orig.antiSnipe,
		verifyAbove: orig.verifyAbove,
		status:      StatusListed,
		relistOf:    orig.id,
		relistGen:   orig.relistGen + 1,
		version:     1,
	}
	a.record(AuctionListed{OccurredAt: now})
	a.record(AuctionRelisted{
		OriginalID: orig.id,
		NewID:      newID,
		StartsAt:   window.StartsAt(),
		EndsAt:     window.EndsAt(),
		OccurredAt: now,
	})
	return a, nil
}

// PlaceBid runs the fixed guard cascade (§2.1) and atomically promotes
// the bid to leading, demoting the previous leader to runner-up. A bid
// inside the anti-snipe window extends the deadline. The accepted Bid
// is returned so the repository can append it to the history log in
// the same transaction.
func (a *Auction) PlaceBid(id BidID, b Bidder, amount Money, now time.Time) (Bid, error) {
	if b.IsZero() {
		return Bid{}, ErrInvalidID
	}

	// Guard cascade — order is fixed by §2.1.
	if a.status != StatusListed || !a.window.IsOpenAt(now) {
		return Bid{}, ErrAuctionNotOpen
	}
	if b.ID().UUID() == a.seller.UUID() {
		return Bid{}, ErrSellerCannotBid
	}
	if !a.leadingBid.IsZero() && a.leadingBid.Bidder() == b.ID() {
		return Bid{}, ErrLeaderCannotOutbidSelf
	}
	if amount.Currency() != a.startPrice.Currency() {
		return Bid{}, ErrCurrencyMismatch
	}
	if meetsMin, err := amount.GTE(MinimalNextBid(*a)); err != nil || !meetsMin {
		return Bid{}, ErrBidBelowMinimum
	}
	if !b.Verified() {
		needsVerification, err := amount.GTE(a.verifyAbove)
		if err != nil || needsVerification {
			return Bid{}, ErrVerificationRequired
		}
	}

	bid, err := NewBid(id, b.ID(), amount, now)
	if err != nil {
		return Bid{}, err
	}

	outbid := a.leadingBid
	a.runnerUpBid = a.leadingBid
	a.leadingBid = bid
	a.bidCount++

	extended := false
	if a.antiSnipe.TriggersAt(now, a.window.EndsAt()) && a.extensionsUsed < a.antiSnipe.MaxExtensions() {
		a.window = a.window.ExtendedBy(a.antiSnipe.Extension())
		a.extensionsUsed++
		extended = true
		a.record(AuctionExtended{NewEndsAt: a.window.EndsAt(), OccurredAt: now})
	}

	a.record(BidPlaced{
		Bid:        bid,
		Outbid:     outbid,
		NewEndsAt:  a.window.EndsAt(),
		Extended:   extended,
		OccurredAt: now,
	})
	return bid, nil
}

// Cancel withdraws a listing before the first bid; only the seller may
// do it (authorization is the pure domain rule CanSellerManageAuction).
func (a *Auction) Cancel(by SellerID, now time.Time) error {
	if err := CanSellerManageAuction(by, *a); err != nil {
		return err
	}
	switch a.status {
	case StatusCancelled:
		return ErrAuctionCancelled
	case StatusClosed:
		return ErrAlreadyClosed
	case StatusListed:
		// proceed
	default:
		panic("auction: unknown status " + a.status.String())
	}
	if a.HasBids() {
		return ErrAuctionHasBids
	}
	a.status = StatusCancelled
	a.record(AuctionCancelled{OccurredAt: now})
	return nil
}

// Close hammers the auction once the (possibly extended) window has
// elapsed. ErrBiddingStillOpen is critical for the worker race: a bid
// may have extended the window after the candidate scan.
func (a *Auction) Close(now time.Time) (ClosingResult, error) {
	switch a.status {
	case StatusClosed:
		return ClosingResult{}, ErrAlreadyClosed
	case StatusCancelled:
		return ClosingResult{}, ErrAuctionCancelled
	case StatusListed:
		// proceed
	default:
		panic("auction: unknown status " + a.status.String())
	}
	if now.Before(a.window.EndsAt()) {
		return ClosingResult{}, ErrBiddingStillOpen
	}

	sold := !a.leadingBid.IsZero() && a.reserve.MetBy(a.leadingBid.Amount())
	a.status = StatusClosed
	if sold {
		a.outcome = OutcomeSold
	} else {
		a.outcome = OutcomeNotSold
	}

	result := ClosingResult{
		Outcome:           a.outcome,
		RunnerUp:          a.runnerUpBid,
		RunnerUpQualifies: !a.runnerUpBid.IsZero() && a.reserve.MetBy(a.runnerUpBid.Amount()),
	}
	if sold {
		result.Winner = a.leadingBid
	}
	a.record(AuctionClosed{Result: result, OccurredAt: now})
	return result, nil
}

// AwardToRunnerUp promotes the runner-up to winner after the original
// winner failed to pay (saga command). Idempotent by the
// winnerReassigned marker: a repeat returns ErrWinnerAlreadyReassigned,
// which the handler maps to nil (§2.1).
func (a *Auction) AwardToRunnerUp(now time.Time) (Bid, error) {
	if a.winnerReassigned {
		return Bid{}, ErrWinnerAlreadyReassigned
	}
	switch a.status {
	case StatusCancelled:
		return Bid{}, ErrAuctionCancelled
	case StatusListed:
		return Bid{}, ErrBiddingStillOpen
	case StatusClosed:
		// proceed
	default:
		panic("auction: unknown status " + a.status.String())
	}
	if a.settled {
		if a.outcome == OutcomeNotSold {
			return Bid{}, ErrSaleAlreadyFailed
		}
		return Bid{}, ErrAlreadySettled
	}
	if a.outcome != OutcomeSold || a.runnerUpBid.IsZero() {
		return Bid{}, ErrNoQualifyingRunnerUp
	}

	a.leadingBid = a.runnerUpBid
	a.runnerUpBid = Bid{}
	a.winnerReassigned = true
	a.record(WinnerReassigned{NewWinner: a.leadingBid, OccurredAt: now})
	return a.leadingBid, nil
}

// ConfirmSettlement marks a sold auction as paid-and-settled (saga
// command). Repeat → ErrAlreadySettled → nil in the handler.
func (a *Auction) ConfirmSettlement(now time.Time) error {
	switch a.status {
	case StatusCancelled:
		return ErrAuctionCancelled
	case StatusListed:
		return ErrBiddingStillOpen
	case StatusClosed:
		// proceed
	default:
		panic("auction: unknown status " + a.status.String())
	}
	if a.outcome != OutcomeSold {
		return ErrSaleAlreadyFailed
	}
	if a.settled {
		return ErrAlreadySettled
	}
	a.settled = true
	a.record(SaleSettled{OccurredAt: now})
	return nil
}

// FailSale marks the settlement as failed (saga command): the sale
// outcome flips to NotSold. Repeat → ErrSaleAlreadyFailed → nil in the
// handler; a successfully settled sale cannot fail (ErrAlreadySettled).
func (a *Auction) FailSale(reason FailureReason, now time.Time) error {
	if reason.IsZero() {
		return ErrInvalidFailureReason
	}
	switch a.status {
	case StatusCancelled:
		return ErrAuctionCancelled
	case StatusListed:
		return ErrBiddingStillOpen
	case StatusClosed:
		// proceed
	default:
		panic("auction: unknown status " + a.status.String())
	}
	if a.outcome == OutcomeNotSold {
		return ErrSaleAlreadyFailed
	}
	if a.settled {
		return ErrAlreadySettled
	}
	a.outcome = OutcomeNotSold
	a.settled = true
	a.record(SaleFailed{Reason: reason, OccurredAt: now})
	return nil
}

// --- predicates and typed getters for mapping (no setters, no GetX) ---

func (a Auction) IsOpenAt(now time.Time) bool {
	return a.status == StatusListed && a.window.IsOpenAt(now)
}

func (a Auction) HasBids() bool { return a.bidCount > 0 }

func (a Auction) ID() AuctionID              { return a.id }
func (a Auction) Seller() SellerID           { return a.seller }
func (a Auction) Lot() Lot                   { return a.lot }
func (a Auction) StartPrice() Money          { return a.startPrice }
func (a Auction) Increment() Money           { return a.increment }
func (a Auction) Reserve() ReservePrice      { return a.reserve }
func (a Auction) Window() BiddingWindow      { return a.window }
func (a Auction) AntiSnipe() AntiSnipePolicy { return a.antiSnipe }
func (a Auction) VerifyAbove() Money         { return a.verifyAbove }
func (a Auction) ExtensionsUsed() int        { return a.extensionsUsed }
func (a Auction) Status() Status             { return a.status }
func (a Auction) Outcome() Outcome           { return a.outcome }
func (a Auction) LeadingBid() Bid            { return a.leadingBid }
func (a Auction) RunnerUpBid() Bid           { return a.runnerUpBid }
func (a Auction) WinnerReassigned() bool     { return a.winnerReassigned }
func (a Auction) BidCount() int              { return a.bidCount }
func (a Auction) RelistOf() AuctionID        { return a.relistOf }
func (a Auction) RelistGeneration() int      { return a.relistGen }
func (a Auction) Settled() bool              { return a.settled }
func (a Auction) Version() int64             { return a.version }
func (a Auction) EndsAt() time.Time          { return a.window.EndsAt() }

// PullDomainEvents drains recorded events for the persisting adapter.
func (a *Auction) PullDomainEvents() []DomainEvent {
	events := a.events
	a.events = nil
	return events
}

func (a *Auction) record(e DomainEvent) {
	a.events = append(a.events, e)
}

// --- stateless computations — plain package functions (rule 13) ---

// MinimalNextBid is the lowest acceptable bid: the start price when
// there are no bids, otherwise leading + increment.
func MinimalNextBid(a Auction) Money {
	if a.leadingBid.IsZero() {
		return a.startPrice
	}
	minNext, err := a.leadingBid.Amount().Add(a.increment)
	if err != nil {
		// Same-currency is a construction invariant; reaching here is
		// a programmer error, not a business outcome.
		panic("auction: leading bid and increment currency diverged: " + err.Error())
	}
	return minNext
}

// CanSellerManageAuction is the authorization rule as a pure domain
// function (rule 22).
func CanSellerManageAuction(s SellerID, a Auction) error {
	if s != a.seller {
		return ForbiddenAuctionManagementError{Actor: s, Owner: a.seller}
	}
	return nil
}

// UnmarshalFromDatabase rehydrates the aggregate from storage. It is
// the only way to build an Auction outside its factories and lives in
// the domain so adapters cannot bypass field invariants (rule 7/11).
// It performs structural checks only — business validation happened
// when the state was first written.
func UnmarshalFromDatabase(
	id AuctionID,
	seller SellerID,
	lot Lot,
	startPrice, increment Money,
	reserve ReservePrice,
	window BiddingWindow,
	antiSnipe AntiSnipePolicy,
	verifyAbove Money,
	extensionsUsed int,
	status Status,
	outcome Outcome,
	leadingBid, runnerUpBid Bid,
	winnerReassigned bool,
	bidCount int,
	relistOf AuctionID,
	relistGen int,
	settled bool,
	version int64,
) (*Auction, error) {
	if id.IsZero() || seller.IsZero() {
		return nil, ErrInvalidID
	}
	if status.IsZero() {
		return nil, ErrInvalidStatus
	}
	if window.IsZero() {
		return nil, ErrInvalidBiddingWindow
	}
	if version < 1 {
		return nil, errors.New("version must be >= 1")
	}
	return &Auction{
		id:               id,
		seller:           seller,
		lot:              lot,
		startPrice:       startPrice,
		increment:        increment,
		reserve:          reserve,
		window:           window,
		antiSnipe:        antiSnipe,
		verifyAbove:      verifyAbove,
		extensionsUsed:   extensionsUsed,
		status:           status,
		outcome:          outcome,
		leadingBid:       leadingBid,
		runnerUpBid:      runnerUpBid,
		winnerReassigned: winnerReassigned,
		bidCount:         bidCount,
		relistOf:         relistOf,
		relistGen:        relistGen,
		settled:          settled,
		version:          version,
	}, nil
}
