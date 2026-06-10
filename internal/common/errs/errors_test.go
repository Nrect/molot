package errs_test

import (
	"errors"
	"fmt"
	"testing"

	"molot/internal/common/errs"
)

func TestConstructors(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		make     func(slug string) errs.SlugError
		wantKind errs.ErrorKind
	}{
		{"incorrect input", errs.NewIncorrectInputError, errs.ErrorKindIncorrectInput},
		{"forbidden", errs.NewForbiddenError, errs.ErrorKindForbidden},
		{"not found", errs.NewNotFoundError, errs.ErrorKindNotFound},
		{"conflict", errs.NewConflictError, errs.ErrorKindConflict},
		{"unavailable", errs.NewUnavailableError, errs.ErrorKindUnavailable},
		{"unknown", errs.NewUnknownError, errs.ErrorKindUnknown},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := tc.make("some-slug")

			if err.Slug != "some-slug" {
				t.Errorf("Slug = %q, want %q", err.Slug, "some-slug")
			}
			if err.Kind != tc.wantKind {
				t.Errorf("Kind = %v, want %v", err.Kind, tc.wantKind)
			}
			if err.Error() != "some-slug" {
				t.Errorf("Error() = %q, want %q", err.Error(), "some-slug")
			}
			if got := errs.KindFromError(err); got != tc.wantKind {
				t.Errorf("KindFromError = %v, want %v", got, tc.wantKind)
			}
		})
	}
}

func TestErrorWrapping(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("auction is already closed")

	t.Run("WithCause exposes the cause to errors.Is", func(t *testing.T) {
		t.Parallel()

		err := errs.NewConflictError("already-closed").WithCause(sentinel)

		if !errors.Is(err, sentinel) {
			t.Error("errors.Is(err, sentinel) = false, want true")
		}
		if want := "already-closed: auction is already closed"; err.Error() != want {
			t.Errorf("Error() = %q, want %q", err.Error(), want)
		}
		if !errors.Is(err.Unwrap(), sentinel) {
			t.Error("Unwrap() does not yield the cause")
		}
	})

	t.Run("errors.As finds SlugError through an outer wrap", func(t *testing.T) {
		t.Parallel()

		err := fmt.Errorf("handling command: %w", errs.NewForbiddenError("not-seller"))

		var slugErr errs.SlugError
		if !errors.As(err, &slugErr) {
			t.Fatal("errors.As did not find SlugError")
		}
		if slugErr.Slug != "not-seller" || slugErr.Kind != errs.ErrorKindForbidden {
			t.Errorf("got %+v, want slug=not-seller kind=forbidden", slugErr)
		}
	})

	t.Run("errors.Is matches by slug and kind ignoring cause", func(t *testing.T) {
		t.Parallel()

		err := errs.NewNotFoundError("invoice-not-found").WithCause(sentinel)

		if !errors.Is(err, errs.NewNotFoundError("invoice-not-found")) {
			t.Error("same slug+kind must match regardless of cause")
		}
		if errors.Is(err, errs.NewConflictError("invoice-not-found")) {
			t.Error("different kind must not match")
		}
		if errors.Is(err, errs.NewNotFoundError("auction-not-found")) {
			t.Error("different slug must not match")
		}
	})
}

func TestKindFromError(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		err  error
		want errs.ErrorKind
	}{
		{"nil chain without slug error", errors.New("plain"), errs.ErrorKindUnknown},
		{"wrapped slug error", fmt.Errorf("outer: %w", errs.NewUnavailableError("psp-unavailable")), errs.ErrorKindUnavailable},
		{"zero-value kind classifies as unknown", errs.SlugError{Slug: "x"}, errs.ErrorKindUnknown},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := errs.KindFromError(tc.err); got != tc.want {
				t.Errorf("KindFromError = %v, want %v", got, tc.want)
			}
		})
	}
}
