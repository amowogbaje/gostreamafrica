package auth

import (
	"context"
	"errors"
	"net/http"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"streamafrica/api/internal/platform/httpx"
)

// ErrForbidden means the caller is authenticated but may not touch this resource.
var ErrForbidden = errors.New("forbidden")

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// TitleOwnerLookup returns the creator id of a title, or ErrNotFound.
type TitleOwnerLookup func(ctx context.Context, titleID string) (creatorID string, err error)

// PGTitleOwner is the Postgres TitleOwnerLookup.
func PGTitleOwner(pool *pgxpool.Pool) TitleOwnerLookup {
	return func(ctx context.Context, titleID string) (string, error) {
		if !uuidRE.MatchString(titleID) {
			return "", ErrNotFound
		}
		var creator string
		err := pool.QueryRow(ctx, `SELECT creator_id::text FROM titles WHERE id = $1::uuid`, titleID).Scan(&creator)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return creator, err
	}
}

// AssertOwner allows the owner and admins.
func AssertOwner(p Principal, ownerID string) error {
	if p.Role == RoleAdmin || (ownerID != "" && p.UserID == ownerID) {
		return nil
	}
	return ErrForbidden
}

// AssertTitleOwner checks that p may modify the title: the creator who owns it, or an admin.
// It returns ErrNotFound for an unknown title and ErrForbidden for someone else's title.
func AssertTitleOwner(ctx context.Context, lookup TitleOwnerLookup, p Principal, titleID string) error {
	owner, err := lookup(ctx, titleID)
	if err != nil {
		return err
	}
	return AssertOwner(p, owner)
}

// RequireTitleOwner is middleware form of AssertTitleOwner. titleID extracts the title id from
// the request (for example r.PathValue("id")). Use it after Authenticate and RequireRole.
func RequireTitleOwner(lookup TitleOwnerLookup, titleID func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := PrincipalFrom(r.Context())
			if !ok {
				unauthorized(w, r, "unauthenticated", "authentication required")
				return
			}
			switch err := AssertTitleOwner(r.Context(), lookup, p, titleID(r)); {
			case errors.Is(err, ErrNotFound):
				httpx.WriteError(w, r, http.StatusNotFound, "not_found", "title not found")
			case errors.Is(err, ErrForbidden):
				httpx.WriteError(w, r, http.StatusForbidden, "forbidden", "you do not own this title")
			case err != nil:
				httpx.WriteError(w, r, http.StatusInternalServerError, "internal_error", "internal server error")
			default:
				next.ServeHTTP(w, r)
			}
		})
	}
}
