package auction

import "github.com/google/uuid"

// Actor is the acting user behind a user-driven Repository.Update.
// It exists so a mutation's principal is an explicit typed parameter
// (BOOK_AUDIT rule 21) and so system flows are forced through the
// loudly named UpdateAsSystem instead of a fake user (rule 23).
type Actor struct {
	id uuid.UUID
}

func NewActor(id uuid.UUID) (Actor, error) {
	if id == uuid.Nil {
		return Actor{}, ErrInvalidID
	}
	return Actor{id: id}, nil
}

// ActorFromSeller and ActorFromBidder lift typed principals into the
// repository's Actor parameter.
func ActorFromSeller(s SellerID) Actor { return Actor{id: s.UUID()} }
func ActorFromBidder(b BidderID) Actor { return Actor{id: b.UUID()} }

func (a Actor) IsZero() bool    { return a == Actor{} }
func (a Actor) UUID() uuid.UUID { return a.id }
func (a Actor) String() string  { return a.id.String() }
