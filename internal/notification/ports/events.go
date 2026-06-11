// Package ports wires the notification context into the platform
// event bus. The context deliberately has no domain or app layer:
// every subscription is a trivial "event → template → send" pipeline
// (BOOK_AUDIT rule 33 exception; rationale and review trigger live in
// internal/notification/README.md), so this package holds both the
// §4.3 subscriptions and the consumer-side interfaces the handlers
// depend on.
package ports

import (
	"context"
	"fmt"
	"time"

	"github.com/ThreeDotsLabs/watermill/components/cqrs"

	auctionevents "molot/internal/auction/events"
	billingevents "molot/internal/billing/events"
	cwatermill "molot/internal/common/watermill"
	participantevents "molot/internal/participant/events"
)

// Notification kinds — the kind component of the sent_notifications
// dedup key. One event may fan out to several (kind, recipient) slots,
// each delivered and deduplicated independently.
const (
	kindOutbidNotice      = "outbid_notice"
	kindAuctionWon        = "auction_won"
	kindAuctionClosed     = "auction_closed"
	kindSecondChanceOffer = "second_chance_offer"
	kindAuctionRelisted   = "auction_relisted"
	kindSaleSettled       = "sale_settled"
	kindSaleFailed        = "sale_failed"
	kindPaymentDueNotice  = "payment_due_notice"
	kindPaymentReceipt    = "payment_receipt"
)

// EmailSender delivers one rendered notification email. Sending is not
// transactional: it happens strictly after the delivery slot has been
// committed (see deliver).
type EmailSender interface {
	Send(ctx context.Context, to, subject, body string) error
}

// RecipientDirectory is the participant_id → (email, display name)
// mini-projection fed by participant-events. Looking up an unknown
// participant returns an error: that is projection lag, and the bus
// redelivers the notification until the registration is consumed.
type RecipientDirectory interface {
	UpsertRecipient(ctx context.Context, participantID, email, displayName string) error
	RecipientByID(ctx context.Context, participantID string) (email, displayName string, err error)
}

// SellerDirectory remembers which seller listed an auction:
// SaleSettledV1/SaleFailedV1 address the seller but do not carry the
// seller id, so it is captured from AuctionListedV1/AuctionClosedV1.
type SellerDirectory interface {
	RememberSeller(ctx context.Context, auctionID, sellerID string) error
	SellerOf(ctx context.Context, auctionID string) (sellerID string, err error)
}

// SentLog is the idempotency ledger keyed by (event_id, kind,
// recipient_id): FirstDelivery records the slot and reports whether
// this call was the first to do so. The record commits before the
// email is sent — the at-most-once-after-dedup trade-off is documented
// in the context README.
type SentLog interface {
	FirstDelivery(ctx context.Context, eventID, kind, recipientID string) (first bool, err error)
}

// EventHandlers is the full subscription surface of the notification
// context (ARCHITECTURE.md §4.3). Handlers are stateless and safe for
// concurrent use as long as their dependencies are.
type EventHandlers struct {
	emails     EmailSender
	recipients RecipientDirectory
	sellers    SellerDirectory
	sentLog    SentLog
}

// NewEventHandlers panics on a nil dependency: a half-wired consumer
// must fail at composition time, not on the first event.
func NewEventHandlers(
	emails EmailSender,
	recipients RecipientDirectory,
	sellers SellerDirectory,
	sentLog SentLog,
) EventHandlers {
	if emails == nil {
		panic("notification.NewEventHandlers: nil EmailSender")
	}
	if recipients == nil {
		panic("notification.NewEventHandlers: nil RecipientDirectory")
	}
	if sellers == nil {
		panic("notification.NewEventHandlers: nil SellerDirectory")
	}
	if sentLog == nil {
		panic("notification.NewEventHandlers: nil SentLog")
	}
	return EventHandlers{emails: emails, recipients: recipients, sellers: sellers, sentLog: sentLog}
}

// Handlers returns every subscription of the context. The handler name
// doubles as the consumer group, giving each subscription an
// independent offset on its topic.
func (h EventHandlers) Handlers() []cqrs.EventHandler {
	return []cqrs.EventHandler{
		cqrs.NewEventHandler("OnParticipantRegisteredUpsertRecipient", h.onParticipantRegistered),
		cqrs.NewEventHandler("OnParticipantVerified", h.onParticipantVerified),
		cqrs.NewEventHandler("OnBidPlacedNotifyOutbid", h.onBidPlaced),
		cqrs.NewEventHandler("OnAuctionListedNotifyRelist", h.onAuctionListed),
		cqrs.NewEventHandler("OnAuctionClosedNotifyOutcome", h.onAuctionClosed),
		cqrs.NewEventHandler("OnWinnerReassignedOfferSecondChance", h.onWinnerReassigned),
		cqrs.NewEventHandler("OnSaleSettledNotifySeller", h.onSaleSettled),
		cqrs.NewEventHandler("OnSaleFailedNotifySeller", h.onSaleFailed),
		cqrs.NewEventHandler("OnInvoiceIssuedNotifyPaymentDue", h.onInvoiceIssued),
		cqrs.NewEventHandler("OnInvoicePaidSendReceipt", h.onInvoicePaid),
	}
}

// subscribeTopics maps every subscribed event name to its source
// topic. Names come from the shared Marshaler, so they cannot drift
// from what the processor sees on the wire.
var subscribeTopics = map[string]string{
	cwatermill.Marshaler.Name(participantevents.ParticipantRegisteredV1{}): participantevents.Topic,
	cwatermill.Marshaler.Name(participantevents.ParticipantVerifiedV1{}):   participantevents.Topic,
	cwatermill.Marshaler.Name(auctionevents.BidPlacedV1{}):                 auctionevents.Topic,
	cwatermill.Marshaler.Name(auctionevents.AuctionListedV1{}):             auctionevents.Topic,
	cwatermill.Marshaler.Name(auctionevents.AuctionClosedV1{}):             auctionevents.Topic,
	cwatermill.Marshaler.Name(auctionevents.WinnerReassignedV1{}):          auctionevents.Topic,
	cwatermill.Marshaler.Name(auctionevents.SaleSettledV1{}):               auctionevents.Topic,
	cwatermill.Marshaler.Name(auctionevents.SaleFailedV1{}):                auctionevents.Topic,
	cwatermill.Marshaler.Name(billingevents.InvoiceIssuedV1{}):             billingevents.Topic,
	cwatermill.Marshaler.Name(billingevents.InvoicePaidV1{}):               billingevents.Topic,
}

// SubscribeTopicFor is the GenerateSubscribeTopic of the notification
// event processor. Registration happens once at startup, so an unknown
// event name is a programming error and panics loudly.
func SubscribeTopicFor(eventName string) string {
	topic, ok := subscribeTopics[eventName]
	if !ok {
		panic("notification: no subscribe topic registered for event " + eventName)
	}
	return topic
}

// --- participant-events -------------------------------------------------

func (h EventHandlers) onParticipantRegistered(ctx context.Context, e *participantevents.ParticipantRegisteredV1) error {
	if err := h.recipients.UpsertRecipient(ctx, e.ParticipantID, e.Email, e.DisplayName); err != nil {
		return fmt.Errorf("project registered participant %s: %w", e.ParticipantID, err)
	}
	return nil
}

// onParticipantVerified keeps the §4.3 subscription wired but is a
// deliberate no-op: the V1 payload carries only the participant id,
// and the recipients row (email, display name) is owned by
// OnParticipantRegisteredUpsertRecipient. No verified flag is stored —
// no notice depends on it, and an unused column would be code for the
// future (BOOK_AUDIT rule 54).
func (h EventHandlers) onParticipantVerified(_ context.Context, _ *participantevents.ParticipantVerifiedV1) error {
	return nil
}

// --- auction-events -----------------------------------------------------

func (h EventHandlers) onBidPlaced(ctx context.Context, e *auctionevents.BidPlacedV1) error {
	if e.OutbidBidderID == "" {
		return nil // first bid: nobody was outbid
	}
	return h.deliver(ctx, e.EventID, kindOutbidNotice, e.OutbidBidderID,
		fmt.Sprintf("You have been outbid on auction %s", e.AuctionID),
		fmt.Sprintf("a new leading bid of %s was placed on auction %s. Bid again before %s to get back in the race.",
			formatMinor(e.AmountMinor, e.Currency), e.AuctionID, e.NewEndsAt.Format(time.RFC3339)))
}

func (h EventHandlers) onAuctionListed(ctx context.Context, e *auctionevents.AuctionListedV1) error {
	// Every listing feeds the seller directory; SaleSettledV1 and
	// SaleFailedV1 need it because they do not carry the seller id.
	if err := h.sellers.RememberSeller(ctx, e.AuctionID, e.SellerID); err != nil {
		return fmt.Errorf("remember seller of auction %s: %w", e.AuctionID, err)
	}
	if e.RelistGeneration == 0 {
		return nil // a fresh listing needs no notice (§4.3: relist-gen>0 only)
	}
	return h.deliver(ctx, e.EventID, kindAuctionRelisted, e.SellerID,
		fmt.Sprintf("Your lot has been relisted as auction %s", e.AuctionID),
		fmt.Sprintf("the previous sale fell through, so your lot was automatically relisted as auction %s. Bidding runs from %s to %s.",
			e.AuctionID, e.StartsAt.Format(time.RFC3339), e.EndsAt.Format(time.RFC3339)))
}

func (h EventHandlers) onAuctionClosed(ctx context.Context, e *auctionevents.AuctionClosedV1) error {
	// Belt to the listing handler's suspenders: the closed event also
	// carries the seller id, so the directory heals even if the listing
	// subscription lags.
	if err := h.sellers.RememberSeller(ctx, e.AuctionID, e.SellerID); err != nil {
		return fmt.Errorf("remember seller of auction %s: %w", e.AuctionID, err)
	}

	// One event, up to two recipients ("won" to the winner, "closed" to
	// the seller). Each notice owns its (event_id, kind, recipient_id)
	// dedup slot, so a redelivery after a partial crash sends only the
	// missing one.
	if e.Outcome == "sold" && e.WinnerID != "" {
		if err := h.deliver(ctx, e.EventID, kindAuctionWon, e.WinnerID,
			fmt.Sprintf("You won auction %s", e.AuctionID),
			fmt.Sprintf("congratulations — your bid of %s won auction %s. An invoice will follow shortly.",
				formatMinor(e.HammerPriceMinor, e.Currency), e.AuctionID)); err != nil {
			return err
		}
	}

	sellerNews := fmt.Sprintf("your auction %s closed without a sale: no qualifying bids.", e.AuctionID)
	if e.Outcome == "sold" {
		sellerNews = fmt.Sprintf("your auction %s sold for %s. Settlement (invoice and payment) is now in progress.",
			e.AuctionID, formatMinor(e.HammerPriceMinor, e.Currency))
	}
	return h.deliver(ctx, e.EventID, kindAuctionClosed, e.SellerID,
		fmt.Sprintf("Your auction %s has closed", e.AuctionID), sellerNews)
}

func (h EventHandlers) onWinnerReassigned(ctx context.Context, e *auctionevents.WinnerReassignedV1) error {
	return h.deliver(ctx, e.EventID, kindSecondChanceOffer, e.NewWinnerID,
		fmt.Sprintf("Second-chance offer for auction %s", e.AuctionID),
		fmt.Sprintf("the original winner of auction %s did not pay; the lot is yours for your bid of %s. An invoice will follow — pay it to claim the lot, or decline the offer.",
			e.AuctionID, formatMinor(e.PriceMinor, e.Currency)))
}

func (h EventHandlers) onSaleSettled(ctx context.Context, e *auctionevents.SaleSettledV1) error {
	sellerID, err := h.sellers.SellerOf(ctx, e.AuctionID)
	if err != nil {
		return fmt.Errorf("resolve seller for settled sale %s: %w", e.AuctionID, err)
	}
	return h.deliver(ctx, e.EventID, kindSaleSettled, sellerID,
		fmt.Sprintf("Auction %s is settled", e.AuctionID),
		fmt.Sprintf("the sale of auction %s has settled: the winner paid in full.", e.AuctionID))
}

func (h EventHandlers) onSaleFailed(ctx context.Context, e *auctionevents.SaleFailedV1) error {
	sellerID, err := h.sellers.SellerOf(ctx, e.AuctionID)
	if err != nil {
		return fmt.Errorf("resolve seller for failed sale %s: %w", e.AuctionID, err)
	}
	return h.deliver(ctx, e.EventID, kindSaleFailed, sellerID,
		fmt.Sprintf("The sale of auction %s fell through", e.AuctionID),
		fmt.Sprintf("the sale of auction %s failed (%s).", e.AuctionID, failureText(e.Reason)))
}

// failureText renders the SaleFailedV1 reason. The schema is
// append-only, so an unknown reason is passed through verbatim rather
// than rejected (forward compatibility for integration payloads — this
// is not a domain enum).
func failureText(reason string) string {
	switch reason {
	case "payment_timeout":
		return "the winner did not pay in time"
	case "second_chance_declined":
		return "the runner-up declined the second-chance offer"
	default:
		return reason
	}
}

// --- billing-events -----------------------------------------------------

func (h EventHandlers) onInvoiceIssued(ctx context.Context, e *billingevents.InvoiceIssuedV1) error {
	return h.deliver(ctx, e.EventID, kindPaymentDueNotice, e.DebtorID,
		fmt.Sprintf("Payment due for auction %s", e.AuctionID),
		fmt.Sprintf("invoice %s for auction %s totals %s (hammer price %s + commission %s). Pay before %s or the lot is forfeited.",
			e.InvoiceID, e.AuctionID, formatMinor(e.TotalMinor, e.Currency),
			formatMinor(e.HammerMinor, e.Currency), formatMinor(e.CommissionMinor, e.Currency),
			e.DueAt.Format(time.RFC3339)))
}

func (h EventHandlers) onInvoicePaid(ctx context.Context, e *billingevents.InvoicePaidV1) error {
	return h.deliver(ctx, e.EventID, kindPaymentReceipt, e.DebtorID,
		fmt.Sprintf("Receipt for invoice %s", e.InvoiceID),
		fmt.Sprintf("we received your payment of %s for auction %s. Thank you!",
			formatMinor(e.TotalMinor, e.Currency), e.AuctionID))
}

// --- delivery pipeline ----------------------------------------------------

// deliver sends one notification with deduplicated recording:
//  1. resolve the recipient — a failure here must NOT consume the dedup
//     slot, otherwise the notice would be lost forever;
//  2. record the (event, kind, recipient) slot; only the first recorder
//     proceeds (redeliveries become no-ops);
//  3. send the email.
//
// The record commits before the send: a crash between the two loses
// exactly that email (at-most-once-after-dedup, see the context
// README).
func (h EventHandlers) deliver(ctx context.Context, eventID, kind, recipientID, subject, news string) error {
	email, displayName, err := h.recipients.RecipientByID(ctx, recipientID)
	if err != nil {
		return fmt.Errorf("resolve %s recipient: %w", kind, err)
	}

	first, err := h.sentLog.FirstDelivery(ctx, eventID, kind, recipientID)
	if err != nil {
		return fmt.Errorf("record %s delivery: %w", kind, err)
	}
	if !first {
		return nil // duplicate delivery on the at-least-once bus: already sent
	}

	body := fmt.Sprintf("Hello %s,\n\n%s", displayName, news)
	if err := h.emails.Send(ctx, email, subject, body); err != nil {
		return fmt.Errorf("send %s email: %w", kind, err)
	}
	return nil
}

// formatMinor renders a minor-unit amount for email bodies assuming a
// 2-decimal platform currency (PLATFORM_CURRENCY is EUR-class, §1).
func formatMinor(minor int64, currency string) string {
	return fmt.Sprintf("%d.%02d %s", minor/100, minor%100, currency)
}
