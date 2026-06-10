// Package auth provides the JWT middleware of the HTTP stack and the
// typed user identity ports read from the request context
// (ARCHITECTURE.md §8).
//
// AUTH_MODE=local-hs256 validates HS256 bearer tokens against a shared
// secret — the dev/test mode; GenerateToken issues tokens through the
// exact same claims/code path, so component tests exercise production
// validation. AUTH_MODE=jwks (OIDC/JWKS validation) is a documented
// future: NewMiddleware rejects it with an actionable error, failing
// startup instead of accepting tokens it cannot validate.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"molot/internal/common/errs"
)

// Auth modes, mirroring config.AuthMode values (auth deliberately does
// not import config — the middleware is usable with any string source).
const (
	ModeLocalHS256 = "local-hs256"
	ModeJWKS       = "jwks"
)

// Role is the coarse platform role carried in the JWT "role" claim.
type Role string

const (
	RoleBidder     Role = "bidder"
	RoleSeller     Role = "seller"
	RoleOperations Role = "operations"
)

func (r Role) valid() bool {
	switch r {
	case RoleBidder, RoleSeller, RoleOperations:
		return true
	default:
		return false
	}
}

// User is the authenticated caller. Ports extract it with UserFromCtx
// and pass it to commands as explicit domain-typed fields.
type User struct {
	ID   uuid.UUID
	Role Role
}

type userCtxKey struct{}

// ContextWithUser returns ctx carrying user. The middleware calls it on
// every authenticated request; tests use it to fabricate authenticated
// contexts without HTTP.
func ContextWithUser(ctx context.Context, user User) context.Context {
	return context.WithValue(ctx, userCtxKey{}, user)
}

// UserFromCtx returns the authenticated user placed in ctx by the
// middleware. A missing user means the route was wired outside the auth
// middleware — a programming error, surfaced as ErrorKindUnknown (500).
func UserFromCtx(ctx context.Context) (User, error) {
	user, ok := ctx.Value(userCtxKey{}).(User)
	if !ok {
		return User{}, errs.NewUnknownError("no-user-in-context")
	}
	return user, nil
}

// NewMiddleware builds the JWT-validating chi middleware for the given
// AUTH_MODE. Unauthenticated requests receive
// 401 {"slug":"unauthorized"} — the same JSON shape as
// httperr.RespondWithSlugError.
func NewMiddleware(mode, hs256Secret string) (func(http.Handler) http.Handler, error) {
	switch mode {
	case ModeLocalHS256:
		if hs256Secret == "" {
			return nil, errors.New("auth: AUTH_HS256_SECRET must not be empty when AUTH_MODE=local-hs256")
		}
		return hs256Middleware([]byte(hs256Secret)), nil
	case ModeJWKS:
		return nil, errors.New("auth: AUTH_MODE=jwks is not implemented yet: run with AUTH_MODE=local-hs256 and AUTH_HS256_SECRET (OIDC/JWKS validation is a planned future)")
	default:
		return nil, fmt.Errorf("auth: unknown AUTH_MODE %q (want %q or %q)", mode, ModeLocalHS256, ModeJWKS)
	}
}

// claims is the token payload: sub (uuid) and exp from the registered
// set plus the platform role.
type claims struct {
	Role string `json:"role"`
	jwt.RegisteredClaims
}

func hs256Middleware(secret []byte) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, err := userFromRequest(r, secret)
			if err != nil {
				respondUnauthorized(w)
				return
			}
			next.ServeHTTP(w, r.WithContext(ContextWithUser(r.Context(), user)))
		})
	}
}

func userFromRequest(r *http.Request, secret []byte) (User, error) {
	tokenString, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tokenString == "" {
		return User{}, errors.New("missing bearer token")
	}

	var c claims
	token, err := jwt.ParseWithClaims(tokenString, &c,
		func(*jwt.Token) (any, error) { return secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return User{}, fmt.Errorf("parse token: %w", err)
	}
	if !token.Valid {
		return User{}, errors.New("invalid token")
	}

	id, err := uuid.Parse(c.Subject)
	if err != nil {
		return User{}, fmt.Errorf("sub claim is not a uuid: %w", err)
	}

	role := Role(c.Role)
	if !role.valid() {
		return User{}, fmt.Errorf("unknown role claim %q", c.Role)
	}

	return User{ID: id, Role: role}, nil
}

func respondUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"slug":"unauthorized"}`))
}

// GenerateToken issues an HS256 token for user, valid for ttl. It is
// the dev/test counterpart of the middleware and shares its claims
// layout, so a generated token always round-trips through validation.
// A negative ttl produces an already-expired token (for tests).
func GenerateToken(secret string, user User, ttl time.Duration) (string, error) {
	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims{
		Role: string(user.Role),
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   user.ID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	})

	signed, err := token.SignedString([]byte(secret))
	if err != nil {
		return "", fmt.Errorf("sign token: %w", err)
	}
	return signed, nil
}
