// Package httperr is the single place errors become HTTP responses:
// ports call RespondWithSlugError on any error crossing the app
// boundary and never craft statuses or bodies themselves
// (ARCHITECTURE.md §8).
package httperr

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"molot/internal/common/errs"
)

const internalSlug = "internal-server-error"

// RespondWithSlugError maps the first errs.SlugError in err's chain to
// an HTTP status (IncorrectInput 400, Forbidden 403, NotFound 404,
// Conflict 409, Unavailable 502, Unknown 500) with body
// {"slug": "<slug>"}. Errors without a SlugError respond
// 500 {"slug":"internal-server-error"}. The full error (cause included)
// is logged here; only the slug leaves the process.
func RespondWithSlugError(err error, w http.ResponseWriter, r *http.Request) {
	if err == nil {
		err = errors.New("httperr: RespondWithSlugError called with nil error")
	}

	slug := internalSlug
	var slugErr errs.SlugError
	if errors.As(err, &slugErr) && slugErr.Slug != "" {
		slug = slugErr.Slug
	}

	status := statusFromKind(errs.KindFromError(err))

	logger := slog.Default().With(
		slog.Any("error", err),
		slog.String("slug", slug),
		slog.Int("status", status),
	)
	if status >= http.StatusInternalServerError {
		logger.ErrorContext(r.Context(), "request failed with server error")
	} else {
		logger.WarnContext(r.Context(), "request failed with client error")
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"slug": slug})
}

func statusFromKind(kind errs.ErrorKind) int {
	switch kind {
	case errs.ErrorKindIncorrectInput:
		return http.StatusBadRequest
	case errs.ErrorKindForbidden:
		return http.StatusForbidden
	case errs.ErrorKindNotFound:
		return http.StatusNotFound
	case errs.ErrorKindConflict:
		return http.StatusConflict
	case errs.ErrorKindUnavailable:
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}
