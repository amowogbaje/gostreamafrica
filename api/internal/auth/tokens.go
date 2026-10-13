package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Role is a user's coarse permission level.
type Role string

// The three roles. Registration always creates RoleViewer.
const (
	RoleViewer  Role = "viewer"
	RoleCreator Role = "creator"
	RoleAdmin   Role = "admin"
)

// Valid reports whether r is one of the known roles.
func (r Role) Valid() bool { return r == RoleViewer || r == RoleCreator || r == RoleAdmin }

const (
	jwtIssuer      = "streamafrica"
	jwtAudience    = "streamafrica-api"
	minSecretBytes = 32
	maxJWTLen      = 2048
)

// Errors returned by TokenIssuer.Verify.
var (
	ErrTokenInvalid = errors.New("invalid token")
	ErrTokenExpired = errors.New("token expired")
)

// Claims is what the API trusts from a verified access token.
type Claims struct {
	UserID    string
	Role      Role
	TokenID   string
	ExpiresAt time.Time
}

type accessClaims struct {
	Role Role `json:"role"`
	jwt.RegisteredClaims
}

// TokenIssuer signs and verifies access tokens. Only HS256 is accepted, so "alg: none"
// and algorithm-confusion tokens are rejected.
type TokenIssuer struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time
	parser *jwt.Parser
}

// NewTokenIssuer validates the secret (at least 32 bytes) and builds the issuer.
func NewTokenIssuer(secret []byte, ttl time.Duration, now func() time.Time) (*TokenIssuer, error) {
	if len(secret) < minSecretBytes {
		return nil, fmt.Errorf("jwt secret must be at least %d bytes", minSecretBytes)
	}
	if ttl <= 0 {
		return nil, errors.New("access token ttl must be positive")
	}
	return &TokenIssuer{
		secret: append([]byte(nil), secret...),
		ttl:    ttl,
		now:    now,
		parser: jwt.NewParser(
			jwt.WithValidMethods([]string{"HS256"}),
			jwt.WithIssuer(jwtIssuer),
			jwt.WithAudience(jwtAudience),
			jwt.WithExpirationRequired(),
			jwt.WithLeeway(5*time.Second),
			jwt.WithTimeFunc(now),
		),
	}, nil
}

// TTL is the lifetime of issued access tokens.
func (i *TokenIssuer) TTL() time.Duration { return i.ttl }

// Issue signs an access token for userID with role.
func (i *TokenIssuer) Issue(userID string, role Role) (string, time.Time, error) {
	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		return "", time.Time{}, fmt.Errorf("read random: %w", err)
	}
	now := i.now()
	exp := now.Add(i.ttl)
	claims := accessClaims{
		Role: role,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    jwtIssuer,
			Subject:   userID,
			Audience:  jwt.ClaimStrings{jwtAudience},
			ExpiresAt: jwt.NewNumericDate(exp),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ID:        hex.EncodeToString(jti),
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(i.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign token: %w", err)
	}
	return signed, exp, nil
}

// Verify checks signature, algorithm, issuer, audience and expiry.
func (i *TokenIssuer) Verify(raw string) (Claims, error) {
	if raw == "" || len(raw) > maxJWTLen {
		return Claims{}, ErrTokenInvalid
	}
	var c accessClaims
	tok, err := i.parser.ParseWithClaims(raw, &c, func(*jwt.Token) (any, error) { return i.secret, nil })
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return Claims{}, ErrTokenExpired
		}
		return Claims{}, ErrTokenInvalid
	}
	if !tok.Valid || c.Subject == "" || !c.Role.Valid() {
		return Claims{}, ErrTokenInvalid
	}
	return Claims{UserID: c.Subject, Role: c.Role, TokenID: c.ID, ExpiresAt: c.ExpiresAt.Time}, nil
}
