package auth

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PGStore is the PostgreSQL implementation of Store. All SQL is parameterized.
type PGStore struct {
	pool *pgxpool.Pool
}

// NewPGStore wraps a pool.
func NewPGStore(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

const userColumns = `u.id::text, u.email::text, u.role::text, u.status::text, u.email_verified_at, u.created_at`

func scanUser(row pgx.Row, extra ...any) (User, error) {
	var u User
	var role, status string
	dest := append([]any{&u.ID, &u.Email, &role, &status, &u.EmailVerifiedAt, &u.CreatedAt}, extra...)
	if err := row.Scan(dest...); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, err
	}
	u.Role, u.Status = Role(role), Status(status)
	return u, nil
}

func rollback(ctx context.Context, tx pgx.Tx) { _ = tx.Rollback(ctx) }

// CreateUser inserts the account and its profile in one transaction. A duplicate email
// (case-insensitive) returns ErrEmailTaken.
func (s *PGStore) CreateUser(ctx context.Context, email, passwordHash, displayName string) (Me, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Me{}, err
	}
	defer rollback(ctx, tx)

	var id string
	err = tx.QueryRow(ctx,
		`INSERT INTO users (email, password_hash) VALUES ($1::citext, $2) RETURNING id::text`,
		email, passwordHash).Scan(&id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Me{}, ErrEmailTaken
	}
	if err != nil {
		return Me{}, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO profiles (user_id, display_name) VALUES ($1::uuid, $2)`, id, displayName); err != nil {
		return Me{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Me{}, err
	}
	return s.UserByID(ctx, id)
}

// UserByEmail looks an account up by email (case-insensitive).
func (s *PGStore) UserByEmail(ctx context.Context, email string) (User, string, error) {
	var hash string
	u, err := scanUser(s.pool.QueryRow(ctx,
		`SELECT `+userColumns+`, u.password_hash FROM users u WHERE u.email = $1::citext`, email), &hash)
	return u, hash, err
}

// UserByID returns a user with their profile.
func (s *PGStore) UserByID(ctx context.Context, id string) (Me, error) {
	var m Me
	u, err := scanUser(s.pool.QueryRow(ctx,
		`SELECT `+userColumns+`, p.display_name, p.avatar_url, p.country::text, p.preferred_language
		   FROM users u JOIN profiles p ON p.user_id = u.id WHERE u.id = $1::uuid`, id),
		&m.Profile.DisplayName, &m.Profile.AvatarURL, &m.Profile.Country, &m.Profile.PreferredLanguage)
	m.User = u
	return m, err
}

// UpdatePasswordHash replaces a user's stored hash (used when parameters are upgraded).
func (s *PGStore) UpdatePasswordHash(ctx context.Context, userID, passwordHash string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE users SET password_hash = $2, updated_at = now() WHERE id = $1::uuid`, userID, passwordHash)
	return err
}

// UpdateProfile applies only the fields present in the patch.
func (s *PGStore) UpdateProfile(ctx context.Context, userID string, p ProfilePatch) (Profile, error) {
	var out Profile
	err := s.pool.QueryRow(ctx,
		`UPDATE profiles SET
			display_name       = CASE WHEN $2::boolean THEN $3::text ELSE display_name END,
			avatar_url         = CASE WHEN $4::boolean THEN $5::text ELSE avatar_url END,
			country            = CASE WHEN $6::boolean THEN $7::text ELSE country END,
			preferred_language = CASE WHEN $8::boolean THEN $9::text ELSE preferred_language END,
			updated_at         = now()
		  WHERE user_id = $1::uuid
		  RETURNING display_name, avatar_url, country::text, preferred_language`,
		userID,
		p.DisplayName != nil, p.DisplayName,
		p.AvatarURLSet, p.AvatarURL,
		p.CountrySet, p.Country,
		p.PreferredLanguage != nil, p.PreferredLanguage,
	).Scan(&out.DisplayName, &out.AvatarURL, &out.Country, &out.PreferredLanguage)
	if errors.Is(err, pgx.ErrNoRows) {
		return Profile{}, ErrNotFound
	}
	return out, err
}

// InsertRefreshToken starts a new token family.
func (s *PGStore) InsertRefreshToken(ctx context.Context, userID string, hash []byte, expiresAt, familyExpiresAt time.Time, userAgent, ip string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO refresh_tokens (user_id, token_hash, expires_at, family_expires_at, user_agent, ip)
		 VALUES ($1::uuid, $2, $3, $4, NULLIF($5::text, ''), NULLIF($6::text, '')::inet)`,
		userID, hash, expiresAt, familyExpiresAt, userAgent, ip)
	return err
}

// RotateRefreshToken implements refresh rotation with reuse detection. The row lock on the
// presented token serialises concurrent attempts, so exactly one can win.
func (s *PGStore) RotateRefreshToken(ctx context.Context, oldHash, newHash []byte, now time.Time, ttl time.Duration, userAgent, ip string) (RotateResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RotateResult{}, err
	}
	defer rollback(ctx, tx)

	var id string
	var res RotateResult
	var expiresAt, familyExpires time.Time
	var usedAt, revokedAt *time.Time
	err = tx.QueryRow(ctx,
		`SELECT id::text, user_id::text, family_id::text, expires_at, family_expires_at, used_at, revoked_at
		   FROM refresh_tokens WHERE token_hash = $1 FOR UPDATE`, oldHash,
	).Scan(&id, &res.UserID, &res.FamilyID, &expiresAt, &familyExpires, &usedAt, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return RotateResult{}, ErrNotFound
	}
	if err != nil {
		return RotateResult{}, err
	}

	if revokedAt != nil {
		return res, ErrRefreshInvalid // already revoked (logout or an earlier reuse response)
	}
	if usedAt != nil {
		// A rotated token came back: someone holds a stolen copy. Burn the whole family.
		if _, err := tx.Exec(ctx,
			`UPDATE refresh_tokens SET revoked_at = $2 WHERE family_id = $1::uuid AND revoked_at IS NULL`,
			res.FamilyID, now); err != nil {
			return RotateResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return RotateResult{}, err
		}
		return res, ErrRefreshReused
	}
	if !expiresAt.After(now) {
		return res, ErrRefreshInvalid
	}

	newExpires := now.Add(ttl)
	if newExpires.After(familyExpires) {
		newExpires = familyExpires
	}
	if !newExpires.After(now) {
		return res, ErrRefreshInvalid
	}

	var newID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO refresh_tokens (user_id, family_id, token_hash, expires_at, family_expires_at, user_agent, ip)
		 VALUES ($1::uuid, $2::uuid, $3, $4, $5, NULLIF($6::text, ''), NULLIF($7::text, '')::inet) RETURNING id::text`,
		res.UserID, res.FamilyID, newHash, newExpires, familyExpires, userAgent, ip).Scan(&newID); err != nil {
		return RotateResult{}, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE refresh_tokens SET used_at = $2, replaced_by = $3::uuid WHERE id = $1::uuid`, id, now, newID); err != nil {
		return RotateResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RotateResult{}, err
	}
	res.ExpiresAt = newExpires
	return res, nil
}

// RevokeFamilyOf revokes every active token in the family of the given token.
func (s *PGStore) RevokeFamilyOf(ctx context.Context, tokenHash []byte, now time.Time) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE refresh_tokens SET revoked_at = $2
		  WHERE revoked_at IS NULL
			AND family_id = (SELECT family_id FROM refresh_tokens WHERE token_hash = $1)`, tokenHash, now)
	return err
}

// RevokeAllRefreshTokens revokes every active refresh token of a user.
func (s *PGStore) RevokeAllRefreshTokens(ctx context.Context, userID string, now time.Time) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE refresh_tokens SET revoked_at = $2 WHERE user_id = $1::uuid AND revoked_at IS NULL`, userID, now)
	return err
}

// IssueOneTimeToken stores a token and voids earlier unused ones of the same purpose.
func (s *PGStore) IssueOneTimeToken(ctx context.Context, userID string, purpose TokenPurpose, hash []byte, expiresAt time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	if _, err := tx.Exec(ctx,
		`UPDATE one_time_tokens SET used_at = now()
		  WHERE user_id = $1::uuid AND purpose = $2::token_purpose AND used_at IS NULL`, userID, string(purpose)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO one_time_tokens (user_id, purpose, token_hash, expires_at)
		 VALUES ($1::uuid, $2::token_purpose, $3, $4)`, userID, string(purpose), hash, expiresAt); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// consumeToken marks a valid token used and returns its user. It is the single-use guarantee:
// the UPDATE only matches an unused, unexpired row, so two racing callers cannot both succeed.
func consumeToken(ctx context.Context, tx pgx.Tx, purpose TokenPurpose, hash []byte, now time.Time) (string, error) {
	var userID string
	err := tx.QueryRow(ctx,
		`UPDATE one_time_tokens SET used_at = $3
		  WHERE token_hash = $1 AND purpose = $2::token_purpose AND used_at IS NULL AND expires_at > $3
		  RETURNING user_id::text`, hash, string(purpose), now).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrInvalidToken
	}
	return userID, err
}

// VerifyEmailWithToken consumes the token and marks the email verified.
func (s *PGStore) VerifyEmailWithToken(ctx context.Context, hash []byte, now time.Time) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer rollback(ctx, tx)
	userID, err := consumeToken(ctx, tx, PurposeEmailVerification, hash, now)
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE users SET email_verified_at = COALESCE(email_verified_at, $2), updated_at = $2 WHERE id = $1::uuid`,
		userID, now); err != nil {
		return "", err
	}
	return userID, tx.Commit(ctx)
}

// ResetPasswordWithToken consumes the token, sets the new hash and revokes every refresh token.
func (s *PGStore) ResetPasswordWithToken(ctx context.Context, hash []byte, now time.Time, newPasswordHash string) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer rollback(ctx, tx)
	userID, err := consumeToken(ctx, tx, PurposePasswordReset, hash, now)
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE users SET password_hash = $2, updated_at = $3 WHERE id = $1::uuid`, userID, newPasswordHash, now); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE refresh_tokens SET revoked_at = $2 WHERE user_id = $1::uuid AND revoked_at IS NULL`, userID, now); err != nil {
		return "", err
	}
	return userID, tx.Commit(ctx)
}
