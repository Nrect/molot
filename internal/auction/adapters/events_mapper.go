package adapters

import (
	"fmt"

	"github.com/google/uuid"

	"molot/internal/auction/domain/auction"
	auctionevents "molot/internal/auction/events"
)

// mapDomainEvent translates one rich domain event into its flat
// integration V1 counterpart (§4.2), reading stable attributes off the
// aggregate. A nil return means "domain-only event, not published"
// (AuctionExtended — BidPlacedV1 already carries Extended/NewEndsAt).
// The reserve price never leaves: AuctionClosedV1 ships the ready-made
// RunnerUpQualifies verdict instead.
//
// The default branch panics: DomainEvent is a closed set (rule 12) —
// a new domain event without a mapping decision is a programmer error.
func mapDomainEvent(a *auction.Auction, e auction.DomainEvent) any {
	currency := a.StartPrice().Currency().String()

	switch e := e.(type) {
	case auction.AuctionListed:
		return auctionevents.AuctionListedV1{
			EventID:          uuid.NewString(),
			AuctionID:        a.ID().String(),
			SellerID:         a.Seller().String(),
			Title:            a.Lot().Title(),
			StartPriceMinor:  a.StartPrice().AmountMinor(),
			Currency:         currency,
			StartsAt:         a.Window().StartsAt(),
			EndsAt:           a.Window().EndsAt(),
			RelistGeneration: a.RelistGeneration(),
			OccurredAt:       e.OccurredAt,
		}

	case auction.BidPlaced:
		outbidBidder := ""
		if !e.Outbid.IsZero() {
			outbidBidder = e.Outbid.Bidder().String()
		}
		return auctionevents.BidPlacedV1{
			EventID:        uuid.NewString(),
			AuctionID:      a.ID().String(),
			BidID:          e.Bid.ID().String(),
			BidderID:       e.Bid.Bidder().String(),
			AmountMinor:    e.Bid.Amount().AmountMinor(),
			Currency:       currency,
			BidCount:       a.BidCount(),
			Extended:       e.Extended,
			NewEndsAt:      e.NewEndsAt,
			OutbidBidderID: outbidBidder,
			OccurredAt:     e.OccurredAt,
		}

	case auction.AuctionExtended:
		return nil // domain-only

	case auction.AuctionCancelled:
		return auctionevents.AuctionCancelledV1{
			EventID:    uuid.NewString(),
			AuctionID:  a.ID().String(),
			SellerID:   a.Seller().String(),
			OccurredAt: e.OccurredAt,
		}

	case auction.AuctionClosed:
		winnerID := ""
		var hammer int64
		if !e.Result.Winner.IsZero() {
			winnerID = e.Result.Winner.Bidder().String()
			hammer = e.Result.Winner.Amount().AmountMinor()
		}
		runnerUpID := ""
		var runnerUpAmount int64
		if !e.Result.RunnerUp.IsZero() {
			runnerUpID = e.Result.RunnerUp.Bidder().String()
			runnerUpAmount = e.Result.RunnerUp.Amount().AmountMinor()
		}
		return auctionevents.AuctionClosedV1{
			EventID:             uuid.NewString(),
			AuctionID:           a.ID().String(),
			SellerID:            a.Seller().String(),
			Outcome:             e.Result.Outcome.String(),
			WinnerID:            winnerID,
			HammerPriceMinor:    hammer,
			Currency:            currency,
			RunnerUpBidderID:    runnerUpID,
			RunnerUpAmountMinor: runnerUpAmount,
			RunnerUpQualifies:   e.Result.RunnerUpQualifies,
			RelistGeneration:    a.RelistGeneration(),
			OccurredAt:          e.OccurredAt,
		}

	case auction.WinnerReassigned:
		return auctionevents.WinnerReassignedV1{
			EventID:     uuid.NewString(),
			AuctionID:   a.ID().String(),
			NewWinnerID: e.NewWinner.Bidder().String(),
			PriceMinor:  e.NewWinner.Amount().AmountMinor(),
			Currency:    currency,
			OccurredAt:  e.OccurredAt,
		}

	case auction.AuctionRelisted:
		return auctionevents.AuctionRelistedV1{
			EventID:           uuid.NewString(),
			OriginalAuctionID: e.OriginalID.String(),
			NewAuctionID:      e.NewID.String(),
			StartsAt:          e.StartsAt,
			EndsAt:            e.EndsAt,
			OccurredAt:        e.OccurredAt,
		}

	case auction.SaleSettled:
		return auctionevents.SaleSettledV1{
			EventID:    uuid.NewString(),
			AuctionID:  a.ID().String(),
			WinnerID:   a.LeadingBid().Bidder().String(),
			OccurredAt: e.OccurredAt,
		}

	case auction.SaleFailed:
		return auctionevents.SaleFailedV1{
			EventID:    uuid.NewString(),
			AuctionID:  a.ID().String(),
			Reason:     e.Reason.String(),
			OccurredAt: e.OccurredAt,
		}

	default:
		panic(fmt.Sprintf("adapters: unmapped domain event %T", e))
	}
}
