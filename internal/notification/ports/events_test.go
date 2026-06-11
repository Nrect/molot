// Black-box tests of the notification subscriptions through their
// public registration surface (EventHandlers.Handlers), with recording
// spies for every dependency (BOOK_AUDIT rule 41): no Postgres, no bus.
package ports_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	auctionevents "molot/internal/auction/events"
	billingevents "molot/internal/billing/events"
	cwatermill "molot/internal/common/watermill"
	"molot/internal/notification/ports"
	participantevents "molot/internal/participant/events"
)

// --- recording spies / stubs ---------------------------------------------

type sentEmail struct {
	to      string
	subject string
	body    string
}

type spyEmailSender struct {
	mu   sync.Mutex
	sent []sentEmail
}

func (s *spyEmailSender) Send(_ context.Context, to, subject, body string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, sentEmail{to: to, subject: subject, body: body})
	return nil
}

func (s *spyEmailSender) emails() []sentEmail {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]sentEmail, len(s.sent))
	copy(out, s.sent)
	return out
}

type stubRecipients struct {
	rows map[string][2]string // participant id → (email, display name)
}

func (r *stubRecipients) UpsertRecipient(_ context.Context, participantID, email, displayName string) error {
	r.rows[participantID] = [2]string{email, displayName}
	return nil
}

func (r *stubRecipients) RecipientByID(_ context.Context, participantID string) (string, string, error) {
	row, ok := r.rows[participantID]
	if !ok {
		return "", "", fmt.Errorf("recipient %s is not in the directory yet", participantID)
	}
	return row[0], row[1], nil
}

type stubSellers struct {
	rows map[string]string // auction id → seller id
}

func (s *stubSellers) RememberSeller(_ context.Context, auctionID, sellerID string) error {
	s.rows[auctionID] = sellerID
	return nil
}

func (s *stubSellers) SellerOf(_ context.Context, auctionID string) (string, error) {
	sellerID, ok := s.rows[auctionID]
	if !ok {
		return "", fmt.Errorf("seller of auction %s is not known yet", auctionID)
	}
	return sellerID, nil
}

type fakeSentLog struct {
	seen map[string]bool
}

func (l *fakeSentLog) FirstDelivery(_ context.Context, eventID, kind, recipientID string) (bool, error) {
	key := eventID + "/" + kind + "/" + recipientID
	if l.seen[key] {
		return false, nil
	}
	l.seen[key] = true
	return true, nil
}

// --- fixture ---------------------------------------------------------------

type fixture struct {
	emails     *spyEmailSender
	recipients *stubRecipients
	sellers    *stubSellers
	handlers   ports.EventHandlers
}

func newFixture() *fixture {
	f := &fixture{
		emails:     &spyEmailSender{},
		recipients: &stubRecipients{rows: map[string][2]string{}},
		sellers:    &stubSellers{rows: map[string]string{}},
	}
	f.handlers = ports.NewEventHandlers(f.emails, f.recipients, f.sellers, &fakeSentLog{seen: map[string]bool{}})
	return f
}

func (f *fixture) know(participantID, email, displayName string) {
	f.recipients.rows[participantID] = [2]string{email, displayName}
}

// handle dispatches an event to the handler registered under name —
// the same code path the event processor uses.
func (f *fixture) handle(t *testing.T, name string, event any) error {
	t.Helper()
	for _, eh := range f.handlers.Handlers() {
		if eh.HandlerName() == name {
			return eh.Handle(t.Context(), event)
		}
	}
	t.Fatalf("handler %s is not registered", name)
	return nil
}

// --- subscriptions ----------------------------------------------------------

func TestSubscriptionMatrix(t *testing.T) {
	t.Parallel()

	want := map[string]string{
		"ParticipantRegisteredV1": participantevents.Topic,
		"ParticipantVerifiedV1":   participantevents.Topic,
		"BidPlacedV1":             auctionevents.Topic,
		"AuctionListedV1":         auctionevents.Topic,
		"AuctionClosedV1":         auctionevents.Topic,
		"WinnerReassignedV1":      auctionevents.Topic,
		"SaleSettledV1":           auctionevents.Topic,
		"SaleFailedV1":            auctionevents.Topic,
		"InvoiceIssuedV1":         billingevents.Topic,
		"InvoicePaidV1":           billingevents.Topic,
	}

	got := map[string]string{}
	for _, eh := range newFixture().handlers.Handlers() {
		name := cwatermill.Marshaler.Name(eh.NewEvent())
		got[name] = ports.SubscribeTopicFor(name)
	}
	assert.Equal(t, want, got, "§4.3 subscription matrix drifted")
}

func TestSubscribeTopicFor_PanicsOnUnknownEvent(t *testing.T) {
	t.Parallel()
	assert.Panics(t, func() { ports.SubscribeTopicFor("SomethingElseV1") })
}

func TestNewEventHandlers_PanicsOnNilDependency(t *testing.T) {
	t.Parallel()

	f := newFixture()
	log := &fakeSentLog{seen: map[string]bool{}}

	cases := []struct {
		name string
		make func()
	}{
		{"nil email sender", func() { ports.NewEventHandlers(nil, f.recipients, f.sellers, log) }},
		{"nil recipient directory", func() { ports.NewEventHandlers(f.emails, nil, f.sellers, log) }},
		{"nil seller directory", func() { ports.NewEventHandlers(f.emails, f.recipients, nil, log) }},
		{"nil sent log", func() { ports.NewEventHandlers(f.emails, f.recipients, f.sellers, nil) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Panics(t, tc.make)
		})
	}
}

// --- BidPlacedV1 → outbid notice -------------------------------------------

func bidPlaced(outbid string) *auctionevents.BidPlacedV1 {
	return &auctionevents.BidPlacedV1{
		EventID:        "evt-bid-1",
		AuctionID:      "auc-1",
		BidID:          "bid-2",
		BidderID:       "carol",
		AmountMinor:    150_00,
		Currency:       "EUR",
		BidCount:       2,
		NewEndsAt:      time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
		OutbidBidderID: outbid,
		OccurredAt:     time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
	}
}

func TestOnBidPlaced(t *testing.T) {
	t.Parallel()

	t.Run("outbid bidder gets exactly one notice across redeliveries", func(t *testing.T) {
		t.Parallel()
		f := newFixture()
		f.know("bob", "bob@example.com", "Bob")
		event := bidPlaced("bob")

		require.NoError(t, f.handle(t, "OnBidPlacedNotifyOutbid", event))
		require.NoError(t, f.handle(t, "OnBidPlacedNotifyOutbid", event)) // at-least-once redelivery

		emails := f.emails.emails()
		require.Len(t, emails, 1)
		assert.Equal(t, "bob@example.com", emails[0].to)
		assert.Contains(t, emails[0].subject, "auc-1")
		assert.Contains(t, emails[0].body, "Hello Bob")
		assert.Contains(t, emails[0].body, "150.00 EUR")
	})

	t.Run("first bid notifies nobody", func(t *testing.T) {
		t.Parallel()
		f := newFixture()
		require.NoError(t, f.handle(t, "OnBidPlacedNotifyOutbid", bidPlaced("")))
		assert.Empty(t, f.emails.emails())
	})

	t.Run("unknown recipient fails without burning the dedup slot", func(t *testing.T) {
		t.Parallel()
		f := newFixture()
		event := bidPlaced("bob")

		require.Error(t, f.handle(t, "OnBidPlacedNotifyOutbid", event), "projection lag must surface for redelivery")
		assert.Empty(t, f.emails.emails())

		f.know("bob", "bob@example.com", "Bob") // registration projection caught up
		require.NoError(t, f.handle(t, "OnBidPlacedNotifyOutbid", event))
		require.Len(t, f.emails.emails(), 1, "redelivery must still send: the slot was not consumed by the failure")
	})
}

// --- AuctionClosedV1 → won + closed -----------------------------------------

func auctionClosed(outcome string) *auctionevents.AuctionClosedV1 {
	e := &auctionevents.AuctionClosedV1{
		EventID:    "evt-closed-1",
		AuctionID:  "auc-1",
		SellerID:   "sara",
		Outcome:    outcome,
		Currency:   "EUR",
		OccurredAt: time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC),
	}
	if outcome == "sold" {
		e.WinnerID = "walt"
		e.HammerPriceMinor = 500_00
	}
	return e
}

func TestOnAuctionClosed(t *testing.T) {
	t.Parallel()

	t.Run("sold fans out to two recipients and redelivery adds nothing", func(t *testing.T) {
		t.Parallel()
		f := newFixture()
		f.know("walt", "walt@example.com", "Walt")
		f.know("sara", "sara@example.com", "Sara")
		event := auctionClosed("sold")

		require.NoError(t, f.handle(t, "OnAuctionClosedNotifyOutcome", event))
		require.NoError(t, f.handle(t, "OnAuctionClosedNotifyOutcome", event)) // redelivery

		emails := f.emails.emails()
		require.Len(t, emails, 2, "one event, two recipients: winner and seller")
		assert.Equal(t, "walt@example.com", emails[0].to)
		assert.Contains(t, emails[0].subject, "won")
		assert.Contains(t, emails[0].body, "500.00 EUR")
		assert.Equal(t, "sara@example.com", emails[1].to)
		assert.Contains(t, emails[1].body, "sold for 500.00 EUR")
	})

	t.Run("partial crash resends only the missing notice", func(t *testing.T) {
		t.Parallel()
		f := newFixture()
		f.know("walt", "walt@example.com", "Walt") // seller not registered yet
		event := auctionClosed("sold")

		require.Error(t, f.handle(t, "OnAuctionClosedNotifyOutcome", event), "seller resolution fails after the winner notice")
		require.Len(t, f.emails.emails(), 1, "winner notice already out")

		f.know("sara", "sara@example.com", "Sara")
		require.NoError(t, f.handle(t, "OnAuctionClosedNotifyOutcome", event))

		emails := f.emails.emails()
		require.Len(t, emails, 2, "redelivery must send only the seller notice")
		assert.Equal(t, "walt@example.com", emails[0].to)
		assert.Equal(t, "sara@example.com", emails[1].to)
	})

	t.Run("not sold notifies only the seller", func(t *testing.T) {
		t.Parallel()
		f := newFixture()
		f.know("sara", "sara@example.com", "Sara")

		require.NoError(t, f.handle(t, "OnAuctionClosedNotifyOutcome", auctionClosed("not_sold")))

		emails := f.emails.emails()
		require.Len(t, emails, 1)
		assert.Equal(t, "sara@example.com", emails[0].to)
		assert.Contains(t, emails[0].body, "without a sale")
	})

	t.Run("remembers the seller for later sale events", func(t *testing.T) {
		t.Parallel()
		f := newFixture()
		f.know("sara", "sara@example.com", "Sara")

		require.NoError(t, f.handle(t, "OnAuctionClosedNotifyOutcome", auctionClosed("not_sold")))
		assert.Equal(t, map[string]string{"auc-1": "sara"}, f.sellers.rows)
	})
}

// --- AuctionListedV1 → relist notice + seller projection --------------------

func auctionListed(relistGen int) *auctionevents.AuctionListedV1 {
	return &auctionevents.AuctionListedV1{
		EventID:          "evt-listed-1",
		AuctionID:        "auc-2",
		SellerID:         "sara",
		Title:            "Old clock",
		StartPriceMinor:  100_00,
		Currency:         "EUR",
		StartsAt:         time.Date(2026, 6, 3, 10, 0, 0, 0, time.UTC),
		EndsAt:           time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC),
		RelistGeneration: relistGen,
		OccurredAt:       time.Date(2026, 6, 3, 9, 0, 0, 0, time.UTC),
	}
}

func TestOnAuctionListed(t *testing.T) {
	t.Parallel()

	t.Run("fresh listing sends nothing but feeds the seller directory", func(t *testing.T) {
		t.Parallel()
		f := newFixture()
		require.NoError(t, f.handle(t, "OnAuctionListedNotifyRelist", auctionListed(0)))
		assert.Empty(t, f.emails.emails())
		assert.Equal(t, map[string]string{"auc-2": "sara"}, f.sellers.rows)
	})

	t.Run("relist notifies the seller exactly once", func(t *testing.T) {
		t.Parallel()
		f := newFixture()
		f.know("sara", "sara@example.com", "Sara")
		event := auctionListed(1)

		require.NoError(t, f.handle(t, "OnAuctionListedNotifyRelist", event))
		require.NoError(t, f.handle(t, "OnAuctionListedNotifyRelist", event)) // redelivery

		emails := f.emails.emails()
		require.Len(t, emails, 1)
		assert.Equal(t, "sara@example.com", emails[0].to)
		assert.Contains(t, emails[0].subject, "relisted")
		assert.Contains(t, emails[0].body, "auc-2")
	})
}

// --- WinnerReassignedV1 → second-chance offer --------------------------------

func TestOnWinnerReassigned(t *testing.T) {
	t.Parallel()

	f := newFixture()
	f.know("rita", "rita@example.com", "Rita")
	event := &auctionevents.WinnerReassignedV1{
		EventID:     "evt-reassign-1",
		AuctionID:   "auc-1",
		NewWinnerID: "rita",
		PriceMinor:  450_00,
		Currency:    "EUR",
		OccurredAt:  time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC),
	}

	require.NoError(t, f.handle(t, "OnWinnerReassignedOfferSecondChance", event))
	require.NoError(t, f.handle(t, "OnWinnerReassignedOfferSecondChance", event)) // redelivery

	emails := f.emails.emails()
	require.Len(t, emails, 1)
	assert.Equal(t, "rita@example.com", emails[0].to)
	assert.Contains(t, emails[0].subject, "Second-chance")
	assert.Contains(t, emails[0].body, "450.00 EUR")
}

// --- SaleSettledV1 / SaleFailedV1 → seller -----------------------------------

func TestOnSaleSettled(t *testing.T) {
	t.Parallel()

	t.Run("notifies the seller exactly once", func(t *testing.T) {
		t.Parallel()
		f := newFixture()
		f.sellers.rows["auc-1"] = "sara"
		f.know("sara", "sara@example.com", "Sara")
		event := &auctionevents.SaleSettledV1{EventID: "evt-settled-1", AuctionID: "auc-1", WinnerID: "walt"}

		require.NoError(t, f.handle(t, "OnSaleSettledNotifySeller", event))
		require.NoError(t, f.handle(t, "OnSaleSettledNotifySeller", event)) // redelivery

		emails := f.emails.emails()
		require.Len(t, emails, 1)
		assert.Equal(t, "sara@example.com", emails[0].to)
		assert.Contains(t, emails[0].body, "paid in full")
	})

	t.Run("unknown auction errors for redelivery", func(t *testing.T) {
		t.Parallel()
		f := newFixture()
		event := &auctionevents.SaleSettledV1{EventID: "evt-settled-2", AuctionID: "auc-unknown", WinnerID: "walt"}
		require.Error(t, f.handle(t, "OnSaleSettledNotifySeller", event),
			"seller projection lag must surface so the bus redelivers")
		assert.Empty(t, f.emails.emails())
	})
}

func TestOnSaleFailed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		reason     string
		wantInBody string
	}{
		{"payment timeout", "payment_timeout", "did not pay in time"},
		{"second chance declined", "second_chance_declined", "declined the second-chance offer"},
		{"unknown reason passes through", "alien_reason", "alien_reason"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture()
			f.sellers.rows["auc-1"] = "sara"
			f.know("sara", "sara@example.com", "Sara")
			event := &auctionevents.SaleFailedV1{EventID: "evt-failed-" + tc.reason, AuctionID: "auc-1", Reason: tc.reason}

			require.NoError(t, f.handle(t, "OnSaleFailedNotifySeller", event))
			require.NoError(t, f.handle(t, "OnSaleFailedNotifySeller", event)) // redelivery

			emails := f.emails.emails()
			require.Len(t, emails, 1)
			assert.Equal(t, "sara@example.com", emails[0].to)
			assert.Contains(t, emails[0].body, tc.wantInBody)
		})
	}
}

// --- billing events ----------------------------------------------------------

func TestOnInvoiceIssued(t *testing.T) {
	t.Parallel()

	f := newFixture()
	f.know("walt", "walt@example.com", "Walt")
	event := &billingevents.InvoiceIssuedV1{
		EventID:         "evt-invoice-1",
		InvoiceID:       "inv-1",
		AuctionID:       "auc-1",
		DebtorID:        "walt",
		HammerMinor:     500_00,
		CommissionMinor: 50_00,
		TotalMinor:      550_00,
		Currency:        "EUR",
		DueAt:           time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC),
		Attempt:         1,
	}

	require.NoError(t, f.handle(t, "OnInvoiceIssuedNotifyPaymentDue", event))
	require.NoError(t, f.handle(t, "OnInvoiceIssuedNotifyPaymentDue", event)) // redelivery

	emails := f.emails.emails()
	require.Len(t, emails, 1)
	assert.Equal(t, "walt@example.com", emails[0].to)
	assert.Contains(t, emails[0].body, "550.00 EUR")
	assert.Contains(t, emails[0].body, "2026-06-07T12:00:00Z")
}

func TestOnInvoicePaid(t *testing.T) {
	t.Parallel()

	f := newFixture()
	f.know("walt", "walt@example.com", "Walt")
	event := &billingevents.InvoicePaidV1{
		EventID:    "evt-paid-1",
		InvoiceID:  "inv-1",
		AuctionID:  "auc-1",
		DebtorID:   "walt",
		TotalMinor: 550_00,
		Currency:   "EUR",
	}

	require.NoError(t, f.handle(t, "OnInvoicePaidSendReceipt", event))
	require.NoError(t, f.handle(t, "OnInvoicePaidSendReceipt", event)) // redelivery

	emails := f.emails.emails()
	require.Len(t, emails, 1)
	assert.Equal(t, "walt@example.com", emails[0].to)
	assert.True(t, strings.Contains(emails[0].subject, "Receipt"), "subject %q must mention the receipt", emails[0].subject)
	assert.Contains(t, emails[0].body, "550.00 EUR")
}

// --- participant events --------------------------------------------------------

func TestOnParticipantRegistered(t *testing.T) {
	t.Parallel()

	t.Run("registration makes the participant addressable", func(t *testing.T) {
		t.Parallel()
		f := newFixture()
		registered := &participantevents.ParticipantRegisteredV1{
			EventID:       "evt-reg-1",
			ParticipantID: "bob",
			Email:         "bob@example.com",
			DisplayName:   "Bob",
		}

		require.NoError(t, f.handle(t, "OnParticipantRegisteredUpsertRecipient", registered))
		require.NoError(t, f.handle(t, "OnParticipantRegisteredUpsertRecipient", registered)) // redelivered upsert

		require.NoError(t, f.handle(t, "OnBidPlacedNotifyOutbid", bidPlaced("bob")))
		emails := f.emails.emails()
		require.Len(t, emails, 1)
		assert.Equal(t, "bob@example.com", emails[0].to)
	})
}

func TestOnParticipantVerified_IsNoOp(t *testing.T) {
	t.Parallel()

	f := newFixture()
	event := &participantevents.ParticipantVerifiedV1{EventID: "evt-ver-1", ParticipantID: "bob"}

	require.NoError(t, f.handle(t, "OnParticipantVerified", event))
	assert.Empty(t, f.emails.emails())
	assert.Empty(t, f.recipients.rows, "the V1 payload carries no address data to project")
}
