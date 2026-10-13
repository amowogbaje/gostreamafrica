package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"streamafrica/api/internal/platform/httpx"
)

// Principal is the authenticated caller, taken from a verified access token (no database call).
// Role changes and suspensions take effect when the access token expires (at most its TTL).
type Principal struct {
	UserID  string
	Role    Role
	TokenID string
}

type principalKey struct{}

// PrincipalFrom returns the caller stored by Authenticate.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// WithPrincipal stores p in ctx (used by Authenticate and by tests).
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// Authenticate requires a valid "Authorization: Bearer <access token>" header.
func Authenticate(tokens *TokenIssuer) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			const prefix = "Bearer "
			h := r.Header.Get("Authorization")
			if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
				unauthorized(w, r, "unauthenticated", "authentication required")
				return
			}
			claims, err := tokens.Verify(strings.TrimSpace(h[len(prefix):]))
			switch {
			case errors.Is(err, ErrTokenExpired):
				unauthorized(w, r, "token_expired", "access token expired")
				return
			case err != nil:
				unauthorized(w, r, "invalid_token", "invalid access token")
				return
			}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), Principal{
				UserID: claims.UserID, Role: claims.Role, TokenID: claims.TokenID,
			})))
		})
	}
}

// OptionalAuthenticate lets anonymous requests through and authenticates those that carry a
// bearer token. A token that is present but expired or invalid is rejected, so a client holding
// a stale token learns to refresh instead of silently being treated as anonymous.
func OptionalAuthenticate(tokens *TokenIssuer) func(http.Handler) http.Handler {
	required := Authenticate(tokens)
	return func(next http.Handler) http.Handler {
		withToken := required(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") == "" {
				next.ServeHTTP(w, r)
				return
			}
			withToken.ServeHTTP(w, r)
		})
	}
}

func unauthorized(w http.ResponseWriter, r *http.Request, code, msg string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="streamafrica"`)
	httpx.WriteError(w, r, http.StatusUnauthorized, code, msg)
}

// RequireRole allows only the listed roles. Roles are exact: list RoleAdmin explicitly if
// admins should pass. Use it after Authenticate.
func RequireRole(roles ...Role) func(http.Handler) http.Handler {
	allowed := make(map[Role]struct{}, len(roles))
	for _, r := range roles {
		allowed[r] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := PrincipalFrom(r.Context())
			if !ok {
				unauthorized(w, r, "unauthenticated", "authentication required")
				return
			}
			if _, ok := allowed[p.Role]; !ok {
				httpx.WriteError(w, r, http.StatusForbidden, "forbidden", "you do not have permission to do this")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
