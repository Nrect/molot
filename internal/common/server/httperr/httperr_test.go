package httperr_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"molot/internal/common/errs"
	"molot/internal/common/server/httperr"
)

func TestRespondWithSlugError(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name       string
		err        error
		wantStatus int
		wantSlug   string
	}{
		{"incorrect input", errs.NewIncorrectInputError("invalid-email"), http.StatusBadRequest, "invalid-email"},
		{"forbidden", errs.NewForbiddenError("seller-cannot-bid"), http.StatusForbidden, "seller-cannot-bid"},
		{"not found", errs.NewNotFoundError("auction-not-found"), http.StatusNotFound, "auction-not-found"},
		{"conflict", errs.NewConflictError("bid-below-minimum"), http.StatusConflict, "bid-below-minimum"},
		{"unavailable", errs.NewUnavailableError("psp-unavailable"), http.StatusBadGateway, "psp-unavailable"},
		{"unknown kind", errs.NewUnknownError("boom"), http.StatusInternalServerError, "boom"},
		{
			"slug error with cause keeps slug",
			errs.NewConflictError("already-closed").WithCause(errors.New("db detail")),
			http.StatusConflict,
			"already-closed",
		},
		{
			"wrapped slug error is unwrapped",
			fmt.Errorf("handling command: %w", errs.NewNotFoundError("invoice-not-found")),
			http.StatusNotFound,
			"invoice-not-found",
		},
		{
			"plain error maps to internal-server-error",
			errors.New("nil pointer somewhere"),
			http.StatusInternalServerError,
			"internal-server-error",
		},
		{"nil error is defensive 500", nil, http.StatusInternalServerError, "internal-server-error"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/whatever", nil)

			httperr.RespondWithSlugError(tc.err, rec, req)

			assert.Equal(t, tc.wantStatus, rec.Code)
			assert.JSONEq(t, fmt.Sprintf(`{"slug":%q}`, tc.wantSlug), rec.Body.String())
			assert.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))
		})
	}
}
