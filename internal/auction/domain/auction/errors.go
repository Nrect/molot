package auction

import (
	"errors"
	"fmt"
)

// Invariant violations are exported package-level sentinels (rule 10);
// callers branch with errors.Is and never re-check what the domain
// already guarantees.
var (
	// PlaceBid guard cascade (§2.1, order fixed).
	ErrAuctionNotOpen         = errors.New("auction is not open for bidding")
	ErrSellerCannotBid        = errors.New("seller cannot bid on own auction")
	ErrLeaderCannotOutbidSelf = errors.New("current leader cannot outbid self")
	ErrCurrencyMismatch       = errors.New("currency mismatch")
	ErrBidBelowMinimum        = errors.New("bid is below the minimal next bid")
	ErrVerificationRequired   = errors.New("bid amount requires a verified bidder")

	// Listing.
	ErrUnsupportedCurrency = errors.New("currency is not the platform currency")

	// Lifecycle.
	ErrAuctionHasBids   = errors.New("auction already has bids")
	ErrAlreadyClosed    = errors.New("auction is already closed")
	ErrAuctionCancelled = errors.New("auction is cancelled")
	ErrBiddingStillOpen = errors.New("bidding window is still open")

	// Settlement facade commands ("already in target state" sentinels
	// are mapped to nil by the command handlers — §2.1).
	ErrNoQualifyingRunnerUp    = errors.New("no qualifying runner-up bid")
	ErrWinnerAlreadyReassigned = errors.New("winner has already been reassigned")
	ErrRelistLimitReached      = errors.New("relist limit reached")
	ErrAlreadyRelisted         = errors.New("auction has already been relisted")
	ErrAlreadySettled          = errors.New("sale is already settled")
	ErrSaleAlreadyFailed       = errors.New("sale has already failed")

	// Constructor validation.
	ErrInvalidID              = errors.New("invalid id")
	ErrInvalidCurrency        = errors.New("invalid currency code")
	ErrNegativeAmount         = errors.New("amount must not be negative")
	ErrInvalidLot             = errors.New("lot title must not be blank")
	ErrInvalidStartPrice      = errors.New("start price must be positive")
	ErrInvalidIncrement       = errors.New("increment must be positive")
	ErrInvalidReserve         = errors.New("reserve price must be positive")
	ErrInvalidBiddingWindow   = errors.New("invalid bidding window")
	ErrInvalidAntiSnipePolicy = errors.New("invalid anti-snipe policy")
	ErrInvalidListingRules    = errors.New("invalid listing rules")
	ErrInvalidBid             = errors.New("invalid bid")
	ErrInvalidStatus          = errors.New("invalid auction status")
	ErrInvalidOutcome         = errors.New("invalid auction outcome")
	ErrInvalidFailureReason   = errors.New("invalid failure reason")
)

// NotFoundError reports a missing aggregate; adapters map driver
// not-found (sql.ErrNoRows) into it so driver errors never leak past
// the repository (rule 19).
type NotFoundError struct {
	AuctionID AuctionID
}

func (e NotFoundError) Error() string {
	return fmt.Sprintf("auction %s not found", e.AuctionID)
}

// ForbiddenAuctionManagementError is the authorization verdict of
// CanSellerManageAuction — a typed error so the audit log keeps both
// principals while ports map it to a slug.
type ForbiddenAuctionManagementError struct {
	Actor SellerID
	Owner SellerID
}

func (e ForbiddenAuctionManagementError) Error() string {
	return fmt.Sprintf("seller %s cannot manage auction owned by %s", e.Actor, e.Owner)
}
