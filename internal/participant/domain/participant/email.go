package participant

import (
	"net/mail"
	"strings"
)

// maxEmailLength is the RFC 3696 erratum limit for a full address.
const maxEmailLength = 320

// EmailAddress is a validated, normalized (lower-case, trimmed) email.
// Validation lives here and only here (BOOK_AUDIT rule 8); the UNIQUE
// constraint in the database relies on this normalization.
type EmailAddress struct {
	value string
}

// NewEmailAddress validates raw as a bare RFC 5322 address (no display
// name) and normalizes it to lower case.
func NewEmailAddress(raw string) (EmailAddress, error) {
	normalized := strings.ToLower(strings.TrimSpace(raw))
	if normalized == "" || len(normalized) > maxEmailLength {
		return EmailAddress{}, ErrInvalidEmail
	}

	addr, err := mail.ParseAddress(normalized)
	if err != nil || addr.Name != "" || addr.Address != normalized {
		return EmailAddress{}, ErrInvalidEmail
	}

	return EmailAddress{value: normalized}, nil
}

func (e EmailAddress) String() string { return e.value }

func (e EmailAddress) IsZero() bool { return e == EmailAddress{} }
