package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"molot/internal/common/auth"
	"molot/internal/common/errs"
)

const secret = "test-secret"

func TestNewMiddleware(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name    string
		mode    string
		secret  string
		wantErr bool
	}{
		{"local hs256 with secret", auth.ModeLocalHS256, secret, false},
		{"local hs256 without secret", auth.ModeLocalHS256, "", true},
		{"jwks is a documented future", auth.ModeJWKS, secret, true},
		{"unknown mode", "saml", secret, true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mw, err := auth.NewMiddleware(tc.mode, tc.secret)
			if tc.wantErr {
				require.Error(t, err)
				assert.Nil(t, mw)
				return
			}
			require.NoError(t, err)
			assert.NotNil(t, mw)
		})
	}
}

// serveWith runs one request through the middleware; the inner handler
// reports the user it extracted from the context.
func serveWith(t *testing.T, authorization string) (*httptest.ResponseRecorder, *auth.User) {
	t.Helper()

	mw, err := auth.NewMiddleware(auth.ModeLocalHS256, secret)
	require.NoError(t, err)

	var seen *auth.User
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, err := auth.UserFromCtx(r.Context())
		require.NoError(t, err)
		seen = &user
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/auctions", nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec, seen
}

func TestMiddlewareAcceptsValidToken(t *testing.T) {
	t.Parallel()

	user := auth.User{ID: uuid.New(), Role: auth.RoleBidder}
	token, err := auth.GenerateToken(secret, user, time.Hour)
	require.NoError(t, err)

	rec, seen := serveWith(t, "Bearer "+token)

	assert.Equal(t, http.StatusNoContent, rec.Code)
	require.NotNil(t, seen)
	assert.Equal(t, user, *seen)
}

func TestMiddlewareRejects(t *testing.T) {
	t.Parallel()

	expired, err := auth.GenerateToken(secret, auth.User{ID: uuid.New(), Role: auth.RoleSeller}, -time.Minute)
	require.NoError(t, err)

	wrongSecret, err := auth.GenerateToken("other-secret", auth.User{ID: uuid.New(), Role: auth.RoleBidder}, time.Hour)
	require.NoError(t, err)

	badRole := signToken(t, jwt.MapClaims{
		"sub":  uuid.NewString(),
		"role": "superadmin",
		"exp":  time.Now().Add(time.Hour).Unix(),
	})
	badSubject := signToken(t, jwt.MapClaims{
		"sub":  "not-a-uuid",
		"role": "bidder",
		"exp":  time.Now().Add(time.Hour).Unix(),
	})
	noExpiry := signToken(t, jwt.MapClaims{
		"sub":  uuid.NewString(),
		"role": "bidder",
	})

	testCases := []struct {
		name          string
		authorization string
	}{
		{"missing header", ""},
		{"not a bearer scheme", "Basic dXNlcjpwYXNz"},
		{"garbage token", "Bearer not.a.jwt"},
		{"expired token", "Bearer " + expired},
		{"wrong signing secret", "Bearer " + wrongSecret},
		{"unknown role claim", "Bearer " + badRole},
		{"sub is not a uuid", "Bearer " + badSubject},
		{"missing exp claim", "Bearer " + noExpiry},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec, seen := serveWith(t, tc.authorization)

			assert.Equal(t, http.StatusUnauthorized, rec.Code)
			assert.Nil(t, seen, "handler must not run for unauthenticated requests")
			assert.JSONEq(t, `{"slug":"unauthorized"}`, rec.Body.String())
			assert.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))
		})
	}
}

func signToken(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	require.NoError(t, err)
	return signed
}

func TestUserFromCtx(t *testing.T) {
	t.Parallel()

	t.Run("present", func(t *testing.T) {
		t.Parallel()

		want := auth.User{ID: uuid.New(), Role: auth.RoleOperations}
		ctx := auth.ContextWithUser(context.Background(), want)

		got, err := auth.UserFromCtx(ctx)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	})

	t.Run("absent", func(t *testing.T) {
		t.Parallel()

		_, err := auth.UserFromCtx(context.Background())
		require.Error(t, err)
		assert.Equal(t, errs.ErrorKindUnknown, errs.KindFromError(err))
	})
}
