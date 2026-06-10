// Package errs defines transport-agnostic slug errors.
//
// The app layer wraps domain sentinels into a SlugError at its boundary;
// ports translate SlugError into transport responses (HTTP status + slug
// body) with a single shared helper. No other error type crosses the
// app -> ports boundary.
package errs

import "errors"

// ErrorKind classifies a SlugError for transport mapping
// (HTTP status, gRPC canonical code). Closed enum.
type ErrorKind struct{ kind string }

var (
	ErrorKindUnknown        = ErrorKind{"unknown"}
	ErrorKindIncorrectInput = ErrorKind{"incorrect-input"}
	ErrorKindForbidden      = ErrorKind{"forbidden"}
	ErrorKindNotFound       = ErrorKind{"not-found"}
	ErrorKindConflict       = ErrorKind{"conflict"}
	ErrorKindUnavailable    = ErrorKind{"unavailable"}
)

func (k ErrorKind) String() string {
	if k.kind == "" {
		return "unknown"
	}
	return k.kind
}

// IsZero reports whether the kind was left uninitialized
// (treated as ErrorKindUnknown by consumers).
func (k ErrorKind) IsZero() bool { return k.kind == "" }

// SlugError is the only error type ports map to transport responses.
// Slug is a stable, client-facing identifier (e.g. "bid-below-minimum");
// the wrapped cause is for logs only and never leaves the process.
type SlugError struct {
	Slug string
	Kind ErrorKind

	cause error
}

func (e SlugError) Error() string {
	if e.cause != nil {
		return e.Slug + ": " + e.cause.Error()
	}
	return e.Slug
}

// Unwrap exposes the cause so errors.Is/errors.As see through SlugError
// down to domain sentinels.
func (e SlugError) Unwrap() error { return e.cause }

// Is matches two SlugErrors by Slug and Kind, ignoring the cause, so
// errors.Is(err, errs.NewConflictError("already-paid")) works regardless
// of what was wrapped.
func (e SlugError) Is(target error) bool {
	t, ok := target.(SlugError)
	return ok && t.Slug == e.Slug && t.Kind == e.Kind
}

// WithCause returns a copy of the error wrapping cause.
func (e SlugError) WithCause(cause error) SlugError {
	e.cause = cause
	return e
}

func NewIncorrectInputError(slug string) SlugError {
	return SlugError{Slug: slug, Kind: ErrorKindIncorrectInput}
}

func NewForbiddenError(slug string) SlugError {
	return SlugError{Slug: slug, Kind: ErrorKindForbidden}
}

func NewNotFoundError(slug string) SlugError {
	return SlugError{Slug: slug, Kind: ErrorKindNotFound}
}

func NewConflictError(slug string) SlugError {
	return SlugError{Slug: slug, Kind: ErrorKindConflict}
}

func NewUnavailableError(slug string) SlugError {
	return SlugError{Slug: slug, Kind: ErrorKindUnavailable}
}

func NewUnknownError(slug string) SlugError {
	return SlugError{Slug: slug, Kind: ErrorKindUnknown}
}

// KindFromError extracts the ErrorKind from the first SlugError in err's
// chain; errors without one classify as ErrorKindUnknown.
func KindFromError(err error) ErrorKind {
	var slugErr SlugError
	if errors.As(err, &slugErr) {
		if slugErr.Kind.IsZero() {
			return ErrorKindUnknown
		}
		return slugErr.Kind
	}
	return ErrorKindUnknown
}
