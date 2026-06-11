package auction

import "strings"

// Lot is what is being sold: a title and a free-form description.
type Lot struct {
	title       string
	description string
}

// NewLot requires a non-blank title; the description may be empty.
func NewLot(title, description string) (Lot, error) {
	if strings.TrimSpace(title) == "" {
		return Lot{}, ErrInvalidLot
	}
	return Lot{title: title, description: description}, nil
}

func (l Lot) IsZero() bool        { return l == Lot{} }
func (l Lot) Title() string       { return l.title }
func (l Lot) Description() string { return l.description }
