package command

import (
	"strconv"

	"github.com/google/uuid"

	"molot/internal/settlement/domain/settlement"
)

// Deterministic saga identities (§6.3, §6.6): derived with uuidv5
// (uuid.NewSHA1) from stable namespaces, so every retry and redelivery
// re-computes the SAME id — idempotency by construction, with the
// database UNIQUE constraints as the second line. The namespace
// constants live in the app layer per the architecture; they must never
// change once data exists.
var (
	nsInvoice = uuid.MustParse("0e2f47a1-5b3c-4a8d-9e6f-1c7b2d4a8e90")
	nsRelist  = uuid.MustParse("7c1d9b30-2e4f-4c6a-8b5d-9f0a3e6c1d72")
)

// InvoiceIDForAttempt is the saga's invoice id for the given attempt:
// uuidv5(nsInvoice, auctionID+":"+attempt).
func InvoiceIDForAttempt(auctionID settlement.AuctionID, attempt int) settlement.InvoiceID {
	raw := uuid.NewSHA1(nsInvoice, []byte(auctionID.String()+":"+strconv.Itoa(attempt)))
	id, err := settlement.NewInvoiceID(raw)
	if err != nil {
		// uuid.NewSHA1 never yields uuid.Nil.
		panic("settlement: deterministic invoice id: " + err.Error())
	}
	return id
}

// RelistIDFor is the deterministic id of the replacement auction:
// uuidv5(nsRelist, auctionID) — a retried relist re-creates the same
// listing, which the auction side dedupes by id and UNIQUE(relist_of).
func RelistIDFor(originalID settlement.AuctionID) settlement.AuctionID {
	raw := uuid.NewSHA1(nsRelist, []byte(originalID.String()))
	id, err := settlement.NewAuctionID(raw)
	if err != nil {
		panic("settlement: deterministic relist id: " + err.Error())
	}
	return id
}
