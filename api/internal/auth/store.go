package auth

import (
	"context"
	"errors"
	"time"
)

// Status is an account's state. Suspended accounts cannot sign in or refresh.
type Status string

// Account statuses.
const (
	StatusActive    Status = "active"
	StatusSuspended Status = "suspended"
)

// User is the public view of an account. It never carries the password hash.
type User struct {
	ID              string     `json:"id"`
	Email           string     `json:"email"`
	Role            Role       `json:"role"`
	Status          Status     `json:"status"`
	EmailVerifiedAt *time.Time `json:"email_verified_at"`
	CreatedAt       time.Time  `json:"created_at"`
}

// Profile holds the user's editable details.
type Profile struct {
	DisplayName       string  `json:"display_name"`
	AvatarURL         *string `json:"avatar_url"`
	Country           *string `json:"country"`
	PreferredLanguage string  `json:"preferred_language"`
}

// Me is a user together with their profile.
type Me struct {
	User    User    `json:"user"`
	Profile Profile `json:"profile"`
}

// ProfilePatch lists the profile fields to change. The *Set flags mark fields that may be
// cleared to NULL.
type ProfilePatch struct {
	DisplayName       *string
	AvatarURLSet      bool
	AvatarURL         *string
	CountrySet        bool
	Country           *string
	PreferredLanguage *string
}

// TokenPurpose says what a one-time token may be used for.
type TokenPurpose string

// One-time token purposes.
const (
	PurposeEmailVerification TokenPurpose = "email_verification"
	PurposePasswordReset     TokenPurpose = "password_reset"
)

// RotateResult describes a successful (or reuse-detected) refresh rotation.
type RotateResult struct {
	UserID    string
	FamilyID  string
	ExpiresAt time.Time
}

var (
	// ErrEmailTaken is returned when registering an email that already exists.
	ErrEmailTaken = errors.New("email already registered")
	// ErrNotFound is returned when a user or token does not exist.
	ErrNotFound = errors.New("not found")
	// ErrInvalidToken means a one-time token is unknown, used or expired.
	ErrInvalidToken = errors.New("invalid or expired token")
	// ErrRefreshInvalid means a refresh token is unknown, expired or revoked.
	ErrRefreshInvalid = errors.New("invalid refresh token")
	// ErrRefreshReused means an already-rotated refresh token was presented again.
	ErrRefreshReused = errors.New("refresh token reuse detected")
)

// Store is the persistence the auth service needs. The Postgres version is PGStore.
type Store interface {
	// CreateUser inserts the user and profile in one transaction.
	CreateUser(ctx context.Context, email, passwordHash, displayName string) (Me, error)
	// UserByEmail returns the user and the stored password hash.
	UserByEmail(ctx context.Context, email string) (User, string, error)
	UserByID(ctx context.Context, id string) (Me, error)
	UpdatePasswordHash(ctx context.Context, userID, passwordHash string) error
	UpdateProfile(ctx context.Context, userID string, p ProfilePatch) (Profile, error)

	// InsertRefreshToken starts a new token family.
	InsertRefreshToken(ctx context.Context, userID string, hash []byte, expiresAt, familyExpiresAt time.Time, userAgent, ip string) error
	// RotateRefreshToken atomically marks oldHash used and inserts newHash in the same family.
	// A token that was already used revokes its whole family and returns ErrRefreshReused.
	RotateRefreshToken(ctx context.Context, oldHash, newHash []byte, now time.Time, ttl time.Duration, userAgent, ip string) (RotateResult, error)
	// RevokeFamilyOf revokes every active token in the family of the given token.
	RevokeFamilyOf(ctx context.Context, tokenHash []byte, now time.Time) error
	RevokeAllRefreshTokens(ctx context.Context, userID string, now time.Time) error

	// IssueOneTimeToken stores a token and voids earlier unused ones of the same purpose.
	IssueOneTimeToken(ctx context.Context, userID string, purpose TokenPurpose, hash []byte, expiresAt time.Time) error
	// VerifyEmailWithToken consumes the token and marks the email verified.
	VerifyEmailWithToken(ctx context.Context, hash []byte, now time.Time) (string, error)
	// ResetPasswordWithToken consumes the token, sets the new hash and revokes every refresh token.
	ResetPasswordWithToken(ctx context.Context, hash []byte, now time.Time, newPasswordHash string) (string, error)
}
