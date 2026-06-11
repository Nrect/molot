package participant

import "strings"

// ParticipantID is the typed identity of a participant: a canonical,
// lower-case UUID string. The domain stays stdlib-only, so the
// constructor validates the canonical 8-4-4-4-12 form itself instead of
// importing a uuid library.
type ParticipantID struct {
	value string
}

// NewParticipantID validates raw as a canonical UUID (case-insensitive,
// normalized to lower case). The nil UUID is rejected: a zero identity
// is never a valid participant.
func NewParticipantID(raw string) (ParticipantID, error) {
	normalized := strings.ToLower(strings.TrimSpace(raw))
	if !isCanonicalUUID(normalized) || normalized == nilUUID {
		return ParticipantID{}, ErrInvalidParticipantID
	}
	return ParticipantID{value: normalized}, nil
}

func (id ParticipantID) String() string { return id.value }

func (id ParticipantID) IsZero() bool { return id == ParticipantID{} }

const nilUUID = "00000000-0000-0000-0000-000000000000"

// isCanonicalUUID reports whether s is a lower-case 8-4-4-4-12 UUID.
func isCanonicalUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch i {
		case 8, 13, 18, 23:
			if s[i] != '-' {
				return false
			}
		default:
			c := s[i]
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
	}
	return true
}
