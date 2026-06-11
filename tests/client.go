//go:build component || e2e

// Package tests provides HTTP client helpers for component and e2e tests
// (BOOK_AUDIT rule 44: codegen clients wrapped in *testing.T helpers).
//
// The approach: thin hand-written HTTP helpers rather than a generated
// client. The server already uses oapi-codegen strict-server; generating a
// separate client binary would add build complexity for minimal gain. The
// helpers encode the same request/response contracts and call require.* so
// the caller can chain: auctionID := c.ListAuction(t, ...).
//
// Auth is exercised through the real JWT middleware (no bypass flags):
// FakeBidderJWT / FakeSellerJWT / FakeOperationsJWT issue real HS256
// tokens via auth.GenerateToken (the same code path as validation,
// BOOK_AUDIT rule 44).
package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"molot/internal/common/auth"
)

const jwtTTL = 24 * time.Hour

// Client is a thin HTTP helper bound to a test server base URL.
// All methods call t.Fatal on unexpected HTTP status codes.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
	Secret     string // AUTH_HS256_SECRET used by the test application
}

// NewClient returns a Client pointing at baseURL using the given secret.
func NewClient(baseURL, secret string) *Client {
	return &Client{
		BaseURL:    baseURL,
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
		Secret:     secret,
	}
}

// --- auth token helpers ---------------------------------------------------

// FakeBidderJWT generates a real HS256 token for a bidder role.
func (c *Client) FakeBidderJWT(t *testing.T, userID uuid.UUID) string {
	t.Helper()
	tok, err := auth.GenerateToken(c.Secret, auth.User{ID: userID, Role: auth.RoleBidder}, jwtTTL)
	require.NoError(t, err, "FakeBidderJWT")
	return tok
}

// FakeSellerJWT generates a real HS256 token for a seller role.
func (c *Client) FakeSellerJWT(t *testing.T, userID uuid.UUID) string {
	t.Helper()
	tok, err := auth.GenerateToken(c.Secret, auth.User{ID: userID, Role: auth.RoleSeller}, jwtTTL)
	require.NoError(t, err, "FakeSellerJWT")
	return tok
}

// FakeOperationsJWT generates a real HS256 token for the operations role.
func (c *Client) FakeOperationsJWT(t *testing.T, userID uuid.UUID) string {
	t.Helper()
	tok, err := auth.GenerateToken(c.Secret, auth.User{ID: userID, Role: auth.RoleOperations}, jwtTTL)
	require.NoError(t, err, "FakeOperationsJWT")
	return tok
}

// --- request helpers -------------------------------------------------------

func (c *Client) do(t *testing.T, method, path string, body any, token string) *http.Response {
	t.Helper()
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err, "marshal request body")
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.BaseURL+path, reqBody)
	require.NoError(t, err, "build request")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.HTTPClient.Do(req)
	require.NoError(t, err, "execute request %s %s", method, path)
	return resp
}

func slugFrom(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var e struct {
		Slug string `json:"slug"`
	}
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &e)
	return e.Slug
}

// requireStatusAndDecode reads the body once, asserts the expected status
// (reporting the raw body on mismatch), then decodes the payload into dst.
// Reading before asserting matters: testify evaluates message args eagerly,
// so slugFrom(t, resp) inside the assertion call would close the body even
// when the assertion passes, breaking the subsequent decode.
func requireStatusAndDecode(t *testing.T, resp *http.Response, want int, name string, dst any) {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "%s: read response body", name)
	require.Equal(t, want, resp.StatusCode, "%s: body=%s", name, string(b))
	require.NoError(t, json.Unmarshal(b, dst), "%s: decode JSON: %s", name, string(b))
}

// --- eventually-poll helpers -------------------------------------------------
//
// testify's assert.Eventually runs its condition in a SEPARATE goroutine
// (go checkCond()). require.* calls t.FailNow(), which is runtime.Goexit() —
// it kills that goroutine before it sends its result, so Eventually never
// re-arms its ticker and silently blocks until the whole waitFor budget
// expires ("Condition never satisfied"), even when the system reached the
// desired state milliseconds later.
//
// Polls against read models and the saga are EXPECTED to see transient
// 404s while projections/saga handlers converge (eventual consistency), so
// Eventually conditions must treat a non-200 as "not yet", never as fatal.
// Use the *Poll helpers below inside assert.Eventually conditions and keep
// the require-based accessors for direct, post-convergence assertions.

// pollJSON performs a GET and decodes the body into dst on 200, returning
// true. A non-200 returns false without failing the test (the read model
// has not converged yet). Transport/decode errors are reported non-fatally
// (assert, not require) so the surrounding assert.Eventually keeps control
// of its condition goroutine.
func (c *Client) pollJSON(t *testing.T, path, token string, dst any) bool {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, c.BaseURL+path, nil)
	if !assert.NoError(t, err, "pollJSON: build request %s", path) {
		return false
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.HTTPClient.Do(req)
	if !assert.NoError(t, err, "pollJSON: execute GET %s", path) {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false // not converged yet — let Eventually retry
	}
	b, err := io.ReadAll(resp.Body)
	if !assert.NoError(t, err, "pollJSON: read body %s", path) {
		return false
	}
	return assert.NoError(t, json.Unmarshal(b, dst),
		"pollJSON: decode %s: %s", path, string(b))
}

// AuctionCardPoll is the non-fatal AuctionCard variant for Eventually
// conditions: ok=false while the card projection has no row yet.
func (c *Client) AuctionCardPoll(t *testing.T, auctionID uuid.UUID, token string) (AuctionCardResponse, bool) {
	t.Helper()
	var card AuctionCardResponse
	ok := c.pollJSON(t, fmt.Sprintf("/api/auctions/%s", auctionID), token, &card)
	return card, ok
}

// SettlementStatusPoll is the non-fatal GetSettlementStatus variant for
// Eventually conditions: ok=false while the saga has not started yet.
func (c *Client) SettlementStatusPoll(t *testing.T, auctionID uuid.UUID, opsToken string) (SettlementResponse, bool) {
	t.Helper()
	var s SettlementResponse
	ok := c.pollJSON(t, fmt.Sprintf("/api/auctions/%s/settlement", auctionID), opsToken, &s)
	return s, ok
}

// SellerDashboardPoll is the non-fatal SellerDashboard variant for
// Eventually conditions.
func (c *Client) SellerDashboardPoll(t *testing.T, sellerID uuid.UUID, sellerToken string) (DashboardResponse, bool) {
	t.Helper()
	var dash DashboardResponse
	ok := c.pollJSON(t, fmt.Sprintf("/api/sellers/%s/dashboard", sellerID), sellerToken, &dash)
	return dash, ok
}

// --- participant endpoints -------------------------------------------------

// RegisterParticipantRequest mirrors the OpenAPI schema.
type RegisterParticipantRequest struct {
	ID          uuid.UUID `json:"id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"displayName"`
}

// RegisterParticipant registers a participant (public endpoint, no auth).
// Asserts 204.
func (c *Client) RegisterParticipant(t *testing.T, id uuid.UUID, email, displayName string) {
	t.Helper()
	resp := c.do(t, http.MethodPost, "/participants", RegisterParticipantRequest{
		ID:          id,
		Email:       email,
		DisplayName: displayName,
	}, "")
	defer resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode,
		"RegisterParticipant slug=%s", slugFrom(t, resp))
}

// VerifyParticipant calls the operations-only verification endpoint. Asserts 204.
func (c *Client) VerifyParticipant(t *testing.T, participantID uuid.UUID, opsToken string) {
	t.Helper()
	resp := c.do(t, http.MethodPost,
		fmt.Sprintf("/api/participants/%s/verification", participantID),
		nil, opsToken)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode,
		"VerifyParticipant slug=%s", slugFrom(t, resp))
}

// --- auction endpoints -----------------------------------------------------

// ListAuctionRequest mirrors the OpenAPI schema.
type ListAuctionRequest struct {
	ID                uuid.UUID `json:"id"`
	Title             string    `json:"title"`
	Description       *string   `json:"description,omitempty"`
	StartPriceMinor   int64     `json:"startPriceMinor"`
	IncrementMinor    int64     `json:"incrementMinor"`
	ReservePriceMinor *int64    `json:"reservePriceMinor,omitempty"`
	Currency          string    `json:"currency"`
	StartsAt          time.Time `json:"startsAt"`
	EndsAt            time.Time `json:"endsAt"`
}

// ListAuction creates an auction. Asserts 204.
func (c *Client) ListAuction(t *testing.T, req ListAuctionRequest, sellerToken string) {
	t.Helper()
	resp := c.do(t, http.MethodPost, "/api/auctions", req, sellerToken)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode,
		"ListAuction slug=%s", slugFrom(t, resp))
}

// AuctionCard fetches the auction card. Asserts 200.
type AuctionCardResponse struct {
	AuctionID           uuid.UUID  `json:"auctionId"`
	Title               string     `json:"title"`
	Description         string     `json:"description"`
	SellerID            uuid.UUID  `json:"sellerId"`
	Status              string     `json:"status"`
	Outcome             *string    `json:"outcome,omitempty"`
	StartPriceMinor     int64      `json:"startPriceMinor"`
	CurrentPriceMinor   int64      `json:"currentPriceMinor"`
	MinimalNextBidMinor int64      `json:"minimalNextBidMinor"`
	IncrementMinor      int64      `json:"incrementMinor"`
	Currency            string     `json:"currency"`
	HasReserve          bool       `json:"hasReserve"`
	StartsAt            time.Time  `json:"startsAt"`
	EndsAt              time.Time  `json:"endsAt"`
	ExtensionsUsed      int        `json:"extensionsUsed"`
	BidCount            int        `json:"bidCount"`
	LeaderID            *uuid.UUID `json:"leaderId,omitempty"`
}

func (c *Client) AuctionCard(t *testing.T, auctionID uuid.UUID, token string) AuctionCardResponse {
	t.Helper()
	resp := c.do(t, http.MethodGet,
		fmt.Sprintf("/api/auctions/%s", auctionID), nil, token)
	var card AuctionCardResponse
	requireStatusAndDecode(t, resp, http.StatusOK, "AuctionCard", &card)
	return card
}

// PlaceBidRequest mirrors the OpenAPI schema.
type PlaceBidRequest struct {
	BidID       uuid.UUID `json:"bidId"`
	AmountMinor int64     `json:"amountMinor"`
	Currency    string    `json:"currency"`
}

// PlaceBid places a bid. Asserts 204.
func (c *Client) PlaceBid(t *testing.T, auctionID uuid.UUID, req PlaceBidRequest, bidderToken string) {
	t.Helper()
	resp := c.do(t, http.MethodPost,
		fmt.Sprintf("/api/auctions/%s/bids", auctionID), req, bidderToken)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode,
		"PlaceBid slug=%s", slugFrom(t, resp))
}

// PlaceBidExpect places a bid and returns the HTTP status code (for error-path tests).
func (c *Client) PlaceBidExpect(t *testing.T, auctionID uuid.UUID, req PlaceBidRequest, bidderToken string) int {
	t.Helper()
	resp := c.do(t, http.MethodPost,
		fmt.Sprintf("/api/auctions/%s/bids", auctionID), req, bidderToken)
	defer resp.Body.Close()
	return resp.StatusCode
}

// PlaceBidEventually places a bid, retrying while the bidder-profile
// projection has not caught up with a just-issued verification
// (403 verification-required). The auction context learns about verification
// asynchronously via ParticipantVerifiedV1, so the first qualifying bid after
// VerifyParticipant legitimately races the projection — a real client would
// retry exactly like this. Any other failure fails the test immediately.
func (c *Client) PlaceBidEventually(t *testing.T, auctionID uuid.UUID, req PlaceBidRequest, bidderToken string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp := c.do(t, http.MethodPost,
			fmt.Sprintf("/api/auctions/%s/bids", auctionID), req, bidderToken)
		status := resp.StatusCode
		slug := slugFrom(t, resp) // closes body
		if status == http.StatusNoContent {
			return
		}
		if !(status == http.StatusForbidden && slug == "verification-required") || time.Now().After(deadline) {
			require.Equal(t, http.StatusNoContent, status, "PlaceBid slug=%s", slug)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// --- seller dashboard ------------------------------------------------------

// DashboardItem mirrors the OpenAPI schema.
type DashboardItem struct {
	AuctionID        uuid.UUID `json:"auctionId"`
	Title            string    `json:"title"`
	Status           string    `json:"status"`
	Outcome          *string   `json:"outcome,omitempty"`
	SettlementStatus *string   `json:"settlementStatus,omitempty"`
	HammerPriceMinor int64     `json:"hammerPriceMinor"`
	BidCount         int       `json:"bidCount"`
	EndsAt           time.Time `json:"endsAt"`
}

// DashboardResponse mirrors the OpenAPI schema.
type DashboardResponse struct {
	Items          []DashboardItem `json:"items"`
	ActiveCount    int             `json:"activeCount"`
	SoldTotalMinor int64           `json:"soldTotalMinor"`
	Currency       string          `json:"currency"`
}

// SellerDashboard fetches the seller dashboard. Asserts 200.
func (c *Client) SellerDashboard(t *testing.T, sellerID uuid.UUID, sellerToken string) DashboardResponse {
	t.Helper()
	resp := c.do(t, http.MethodGet,
		fmt.Sprintf("/api/sellers/%s/dashboard", sellerID), nil, sellerToken)
	var dash DashboardResponse
	requireStatusAndDecode(t, resp, http.StatusOK, "SellerDashboard", &dash)
	return dash
}

// --- billing endpoints -----------------------------------------------------

// InvoiceResponse mirrors the OpenAPI schema.
type InvoiceResponse struct {
	InvoiceID       uuid.UUID `json:"invoiceId"`
	AuctionID       uuid.UUID `json:"auctionId"`
	Status          string    `json:"status"`
	HammerMinor     int64     `json:"hammerMinor"`
	CommissionMinor int64     `json:"commissionMinor"`
	TotalMinor      int64     `json:"totalMinor"`
	Currency        string    `json:"currency"`
	DueAt           time.Time `json:"dueAt"`
	Attempt         int       `json:"attempt"`
}

// GetInvoice fetches an invoice. Asserts 200.
func (c *Client) GetInvoice(t *testing.T, invoiceID uuid.UUID, bidderToken string) InvoiceResponse {
	t.Helper()
	resp := c.do(t, http.MethodGet,
		fmt.Sprintf("/api/invoices/%s", invoiceID), nil, bidderToken)
	var inv InvoiceResponse
	requireStatusAndDecode(t, resp, http.StatusOK, "GetInvoice", &inv)
	return inv
}

// GetInvoiceStatus fetches only the HTTP status for an invoice request (error-path tests).
func (c *Client) GetInvoiceStatus(t *testing.T, invoiceID uuid.UUID, bidderToken string) int {
	t.Helper()
	resp := c.do(t, http.MethodGet,
		fmt.Sprintf("/api/invoices/%s", invoiceID), nil, bidderToken)
	defer resp.Body.Close()
	return resp.StatusCode
}

// PendingInvoicesResponse mirrors the OpenAPI schema.
type PendingInvoicesResponse struct {
	Invoices []InvoiceResponse `json:"invoices"`
}

// GetPendingInvoices fetches pending invoices for a bidder. Asserts 200.
func (c *Client) GetPendingInvoices(t *testing.T, bidderID uuid.UUID, bidderToken string) []InvoiceResponse {
	t.Helper()
	resp := c.do(t, http.MethodGet,
		fmt.Sprintf("/api/bidders/%s/invoices?status=pending", bidderID),
		nil, bidderToken)
	var result PendingInvoicesResponse
	requireStatusAndDecode(t, resp, http.StatusOK, "GetPendingInvoices", &result)
	return result.Invoices
}

// PayInvoice pays an invoice. Asserts 204.
func (c *Client) PayInvoice(t *testing.T, invoiceID uuid.UUID, bidderToken string) {
	t.Helper()
	resp := c.do(t, http.MethodPost,
		fmt.Sprintf("/api/invoices/%s/payment", invoiceID), nil, bidderToken)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode,
		"PayInvoice slug=%s", slugFrom(t, resp))
}

// PayInvoiceExpect pays an invoice and returns the HTTP status code.
func (c *Client) PayInvoiceExpect(t *testing.T, invoiceID uuid.UUID, bidderToken string) int {
	t.Helper()
	resp := c.do(t, http.MethodPost,
		fmt.Sprintf("/api/invoices/%s/payment", invoiceID), nil, bidderToken)
	defer resp.Body.Close()
	return resp.StatusCode
}

// --- settlement endpoints --------------------------------------------------

// SettlementResponse mirrors the OpenAPI schema.
type SettlementResponse struct {
	AuctionID         uuid.UUID  `json:"auctionId"`
	State             string     `json:"state"`
	WinnerID          uuid.UUID  `json:"winnerId"`
	HammerMinor       int64      `json:"hammerMinor"`
	Currency          string     `json:"currency"`
	RunnerUpID        *uuid.UUID `json:"runnerUpId,omitempty"`
	RunnerUpMinor     *int64     `json:"runnerUpMinor,omitempty"`
	RunnerUpQualifies bool       `json:"runnerUpQualifies"`
	RelistGeneration  int        `json:"relistGeneration"`
	Attempt           int        `json:"attempt"`
	InvoiceID         *uuid.UUID `json:"invoiceId,omitempty"`
	FailureReason     *string    `json:"failureReason,omitempty"`
}

// GetSettlementStatus fetches the settlement saga status (ops token). Asserts 200.
func (c *Client) GetSettlementStatus(t *testing.T, auctionID uuid.UUID, opsToken string) SettlementResponse {
	t.Helper()
	resp := c.do(t, http.MethodGet,
		fmt.Sprintf("/api/auctions/%s/settlement", auctionID), nil, opsToken)
	var s SettlementResponse
	requireStatusAndDecode(t, resp, http.StatusOK, "GetSettlementStatus", &s)
	return s
}

// DeclineSecondChanceOffer declines the second-chance offer. Asserts 204.
func (c *Client) DeclineSecondChanceOffer(t *testing.T, auctionID uuid.UUID, runnerUpToken string) {
	t.Helper()
	resp := c.do(t, http.MethodPost,
		fmt.Sprintf("/api/auctions/%s/second-chance/decline", auctionID), nil, runnerUpToken)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode,
		"DeclineSecondChanceOffer slug=%s", slugFrom(t, resp))
}

// DeclineSecondChanceOfferExpect declines the second-chance offer and returns the HTTP status.
func (c *Client) DeclineSecondChanceOfferExpect(t *testing.T, auctionID uuid.UUID, runnerUpToken string) int {
	t.Helper()
	resp := c.do(t, http.MethodPost,
		fmt.Sprintf("/api/auctions/%s/second-chance/decline", auctionID), nil, runnerUpToken)
	defer resp.Body.Close()
	return resp.StatusCode
}

// --- health -----------------------------------------------------------------

// WaitForReadyz polls GET /readyz until it returns 200 or the deadline is
// exceeded. Uses a tight loop without sleep — assert.Eventually style.
func (c *Client) WaitForReadyz(t *testing.T, deadline time.Duration) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		resp, err := c.HTTPClient.Get(c.BaseURL + "/readyz")
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			return
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("WaitForReadyz: /readyz did not return 200 within %s", deadline)
}
