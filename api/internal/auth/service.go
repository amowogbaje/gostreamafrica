package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Errors returned by the service.
var (
	// ErrInvalidCredentials is returned for a wrong password or an unknown email (indistinguishable).
	ErrInvalidCredentials = errors.New("invalid credentials")
	// ErrAccountDisabled is returned after correct credentials for a suspended account.
	ErrAccountDisabled = errors.New("account disabled")
	// ErrTrackerUnavailable means the lockout store failed; login fails closed.
	ErrTrackerUnavailable = errors.New("lockout store unavailable")
)

// LockedError means too many failed attempts: wait RetryAfter before trying again.
type LockedError struct{ RetryAfter time.Duration }

func (e *LockedError) Error() string { return "too many failed attempts" }

const tokenBytes = 32

// ClientMeta is what we record about the caller of a token-issuing request.
type ClientMeta struct {
	UserAgent string
	IP        string
}

// Tokens are the credentials handed to a client.
type Tokens struct {
	AccessToken      string
	AccessExpiresAt  time.Time
	RefreshToken     string
	RefreshExpiresAt time.Time
}

// AuthResult is a signed-in user and their tokens.
type AuthResult struct {
	Me     Me
	Tokens Tokens
}

// ServiceConfig holds the service's tunables.
type ServiceConfig struct {
	RefreshTTL         time.Duration
	RefreshAbsoluteTTL time.Duration
	VerifyTTL          time.Duration
	ResetTTL           time.Duration
	WebBaseURL         string
	SyncEmail          bool // send mail inline (tests); production sends in the background
}

// Deps are the collaborators of Service.
type Deps struct {
	Store   Store
	Hasher  *Hasher
	Tokens  *TokenIssuer
	Tracker FailureTracker
	Mailer  Mailer
	Logger  *slog.Logger
	Now     func() time.Time
	Config  ServiceConfig
}

// Service holds the auth business rules.
type Service struct {
	Deps
	dummyHash string // verified against when the email is unknown, to equalise timing
	mail      sync.WaitGroup
}

// NewService builds the service. It hashes one throwaway password at startup.
func NewService(d Deps) (*Service, error) {
	if d.Config.VerifyTTL <= 0 {
		d.Config.VerifyTTL = 24 * time.Hour
	}
	if d.Config.ResetTTL <= 0 {
		d.Config.ResetTTL = time.Hour
	}
	dummy, err := d.Hasher.Hash(context.Background(), "timing-equaliser-not-a-real-password")
	if err != nil {
		return nil, fmt.Errorf("prepare dummy hash: %w", err)
	}
	return &Service{Deps: d, dummyHash: dummy}, nil
}

// Wait blocks until background emails finish. Call it during shutdown.
func (s *Service) Wait() { s.mail.Wait() }

// Register creates an account (role viewer), sends the verification email and signs the user in.
func (s *Service) Register(ctx context.Context, email, password, displayName string, meta ClientMeta) (AuthResult, error) {
	hash, err := s.Hasher.Hash(ctx, password)
	if err != nil {
		return AuthResult{}, fmt.Errorf("hash password: %w", err)
	}
	me, err := s.Store.CreateUser(ctx, email, hash, displayName)
	if err != nil {
		return AuthResult{}, err
	}
	if err := s.sendVerification(ctx, me.User, displayName); err != nil {
		s.Logger.WarnContext(ctx, "could not issue verification token", "user_id", me.User.ID, "error", err.Error())
	}
	tokens, err := s.startSession(ctx, me.User, meta)
	if err != nil {
		return AuthResult{}, err
	}
	return AuthResult{Me: me, Tokens: tokens}, nil
}

// Login verifies credentials after checking the lockout, and opens a session.
func (s *Service) Login(ctx context.Context, email, password string, meta ClientMeta) (AuthResult, error) {
	wait, err := s.Tracker.Locked(ctx, email, meta.IP)
	if err != nil {
		return AuthResult{}, fmt.Errorf("%w: %w", ErrTrackerUnavailable, err)
	}
	if wait > 0 {
		return AuthResult{}, &LockedError{RetryAfter: wait}
	}

	user, hash, err := s.Store.UserByEmail(ctx, email)
	switch {
	case errors.Is(err, ErrNotFound):
		// Burn the same CPU as a real check so response time does not reveal the email is unknown.
		_, _, _ = s.Hasher.Verify(ctx, password, s.dummyHash)
		return AuthResult{}, s.loginFailed(ctx, email, meta)
	case err != nil:
		return AuthResult{}, err
	}

	ok, rehash, err := s.Hasher.Verify(ctx, password, hash)
	switch {
	case errors.Is(err, ErrInvalidHash):
		s.Logger.ErrorContext(ctx, "stored password hash is not valid", "user_id", user.ID)
		return AuthResult{}, s.loginFailed(ctx, email, meta)
	case err != nil:
		return AuthResult{}, fmt.Errorf("verify password: %w", err)
	case !ok:
		return AuthResult{}, s.loginFailed(ctx, email, meta)
	}
	if user.Status != StatusActive {
		return AuthResult{}, ErrAccountDisabled
	}
	if err := s.Tracker.Succeeded(ctx, email, meta.IP); err != nil {
		s.Logger.WarnContext(ctx, "could not reset login failures", "error", err.Error())
	}
	if rehash {
		s.upgradeHash(ctx, user.ID, password)
	}
	me, err := s.Store.UserByID(ctx, user.ID)
	if err != nil {
		return AuthResult{}, err
	}
	tokens, err := s.startSession(ctx, me.User, meta)
	if err != nil {
		return AuthResult{}, err
	}
	return AuthResult{Me: me, Tokens: tokens}, nil
}

func (s *Service) loginFailed(ctx context.Context, email string, meta ClientMeta) error {
	if err := s.Tracker.Failed(ctx, email, meta.IP); err != nil {
		s.Logger.ErrorContext(ctx, "could not record login failure", "error", err.Error())
	}
	return ErrInvalidCredentials
}

func (s *Service) upgradeHash(ctx context.Context, userID, password string) {
	fresh, err := s.Hasher.Hash(ctx, password)
	if err == nil {
		err = s.Store.UpdatePasswordHash(ctx, userID, fresh)
	}
	if err != nil {
		s.Logger.WarnContext(ctx, "password hash upgrade failed", "user_id", userID, "error", err.Error())
	}
}

// Refresh exchanges a refresh token for a new access token and a new refresh token. The old
// refresh token stops working. Presenting an old one again revokes the whole family.
func (s *Service) Refresh(ctx context.Context, rawRefresh string, meta ClientMeta) (AuthResult, error) {
	oldHash, ok := hashOpaque(rawRefresh)
	if !ok {
		return AuthResult{}, ErrRefreshInvalid
	}
	newRaw, newHash, err := newOpaque()
	if err != nil {
		return AuthResult{}, err
	}
	now := s.Now()
	res, err := s.Store.RotateRefreshToken(ctx, oldHash, newHash, now, s.Config.RefreshTTL, meta.UserAgent, meta.IP)
	switch {
	case errors.Is(err, ErrRefreshReused):
		s.Logger.WarnContext(ctx, "SECURITY refresh token reuse detected, family revoked",
			"user_id", res.UserID, "family_id", res.FamilyID, "ip", meta.IP)
		return AuthResult{}, ErrRefreshInvalid
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrRefreshInvalid):
		return AuthResult{}, ErrRefreshInvalid
	case err != nil:
		return AuthResult{}, err
	}

	me, err := s.Store.UserByID(ctx, res.UserID)
	if err != nil {
		return AuthResult{}, err
	}
	if me.User.Status != StatusActive {
		_ = s.Store.RevokeAllRefreshTokens(ctx, me.User.ID, now)
		return AuthResult{}, ErrRefreshInvalid
	}
	access, accessExp, err := s.Tokens.Issue(me.User.ID, me.User.Role)
	if err != nil {
		return AuthResult{}, err
	}
	return AuthResult{Me: me, Tokens: Tokens{
		AccessToken: access, AccessExpiresAt: accessExp, RefreshToken: newRaw, RefreshExpiresAt: res.ExpiresAt,
	}}, nil
}

// Logout revokes the family of the presented refresh token. An unknown token is not an error.
func (s *Service) Logout(ctx context.Context, rawRefresh string) error {
	hash, ok := hashOpaque(rawRefresh)
	if !ok {
		return nil
	}
	return s.Store.RevokeFamilyOf(ctx, hash, s.Now())
}

// LogoutAll revokes every refresh token of the user. Access tokens already issued stay valid
// until they expire (at most the access TTL).
func (s *Service) LogoutAll(ctx context.Context, userID string) error {
	return s.Store.RevokeAllRefreshTokens(ctx, userID, s.Now())
}

// VerifyEmail consumes a verification token.
func (s *Service) VerifyEmail(ctx context.Context, rawToken string) error {
	hash, ok := hashOpaque(rawToken)
	if !ok {
		return ErrInvalidToken
	}
	_, err := s.Store.VerifyEmailWithToken(ctx, hash, s.Now())
	return err
}

// ResendVerification sends a fresh link when the account exists and is unverified. It never
// reveals whether the account exists.
func (s *Service) ResendVerification(ctx context.Context, email string) error {
	user, _, err := s.Store.UserByEmail(ctx, email)
	if errors.Is(err, ErrNotFound) || (err == nil && (user.EmailVerifiedAt != nil || user.Status != StatusActive)) {
		return nil
	}
	if err != nil {
		return err
	}
	me, err := s.Store.UserByID(ctx, user.ID)
	if err != nil {
		return err
	}
	return s.sendVerification(ctx, user, me.Profile.DisplayName)
}

// ForgotPassword emails a reset link when the account exists. It never reveals whether it does.
func (s *Service) ForgotPassword(ctx context.Context, email string) error {
	user, _, err := s.Store.UserByEmail(ctx, email)
	if errors.Is(err, ErrNotFound) || (err == nil && user.Status != StatusActive) {
		return nil
	}
	if err != nil {
		return err
	}
	raw, hash, err := newOpaque()
	if err != nil {
		return err
	}
	if err := s.Store.IssueOneTimeToken(ctx, user.ID, PurposePasswordReset, hash, s.Now().Add(s.Config.ResetTTL)); err != nil {
		return err
	}
	s.send(Message{
		To:      user.Email,
		Subject: "Reset your StreamAfrica password",
		Body: "We received a request to reset your StreamAfrica password.\n\n" +
			"Open this link within 1 hour to choose a new password:\n" +
			s.Config.WebBaseURL + "/reset-password#token=" + raw + "\n\n" +
			"If you did not ask for this, ignore this email. Your password has not changed.\n",
	})
	return nil
}

// ResetPassword sets a new password using a reset token and signs the user out everywhere.
func (s *Service) ResetPassword(ctx context.Context, rawToken, newPassword string) error {
	tokenHash, ok := hashOpaque(rawToken)
	if !ok {
		return ErrInvalidToken
	}
	hash, err := s.Hasher.Hash(ctx, newPassword)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	_, err = s.Store.ResetPasswordWithToken(ctx, tokenHash, s.Now(), hash)
	return err
}

// Me returns the user and profile.
func (s *Service) Me(ctx context.Context, userID string) (Me, error) {
	return s.Store.UserByID(ctx, userID)
}

// UpdateProfile applies a profile patch and returns the fresh account.
func (s *Service) UpdateProfile(ctx context.Context, userID string, p ProfilePatch) (Me, error) {
	if _, err := s.Store.UpdateProfile(ctx, userID, p); err != nil {
		return Me{}, err
	}
	return s.Store.UserByID(ctx, userID)
}

func (s *Service) startSession(ctx context.Context, user User, meta ClientMeta) (Tokens, error) {
	raw, hash, err := newOpaque()
	if err != nil {
		return Tokens{}, err
	}
	now := s.Now()
	expires := now.Add(s.Config.RefreshTTL)
	familyExpires := now.Add(s.Config.RefreshAbsoluteTTL)
	if expires.After(familyExpires) {
		expires = familyExpires
	}
	if err := s.Store.InsertRefreshToken(ctx, user.ID, hash, expires, familyExpires, meta.UserAgent, meta.IP); err != nil {
		return Tokens{}, fmt.Errorf("store refresh token: %w", err)
	}
	access, accessExp, err := s.Tokens.Issue(user.ID, user.Role)
	if err != nil {
		return Tokens{}, err
	}
	return Tokens{AccessToken: access, AccessExpiresAt: accessExp, RefreshToken: raw, RefreshExpiresAt: expires}, nil
}

func (s *Service) sendVerification(ctx context.Context, user User, displayName string) error {
	raw, hash, err := newOpaque()
	if err != nil {
		return err
	}
	if err := s.Store.IssueOneTimeToken(ctx, user.ID, PurposeEmailVerification, hash, s.Now().Add(s.Config.VerifyTTL)); err != nil {
		return err
	}
	s.send(Message{
		To:      user.Email,
		Subject: "Confirm your StreamAfrica email",
		Body: "Hi " + displayName + ",\n\n" +
			"Welcome to StreamAfrica. Confirm your email address within 24 hours:\n" +
			s.Config.WebBaseURL + "/verify-email#token=" + raw + "\n\n" +
			"If you did not create this account, ignore this email.\n",
	})
	return nil
}

// send delivers mail. In production it runs in the background so a slow SMTP server cannot slow
// down (or reveal anything about) the request; failures are logged and recoverable by resending.
func (s *Service) send(m Message) {
	if s.Config.SyncEmail {
		if err := s.Mailer.Send(context.Background(), m); err != nil {
			s.Logger.Error("send email failed", "error", err.Error())
		}
		return
	}
	s.mail.Add(1)
	go func() {
		defer s.mail.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := s.Mailer.Send(ctx, m); err != nil {
			s.Logger.Error("send email failed", "error", err.Error())
		}
	}()
}

// newOpaque returns a random token and its SHA-256 hash. Only the hash is ever stored.
func newOpaque() (raw string, hash []byte, err error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", nil, fmt.Errorf("read random: %w", err)
	}
	raw = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(raw))
	return raw, sum[:], nil
}

// hashOpaque returns the hash of a well-formed token. Malformed tokens are rejected without a database call.
func hashOpaque(token string) ([]byte, bool) {
	if len(token) != base64.RawURLEncoding.EncodedLen(tokenBytes) {
		return nil, false
	}
	if _, err := base64.RawURLEncoding.DecodeString(token); err != nil {
		return nil, false
	}
	sum := sha256.Sum256([]byte(token))
	return sum[:], true
}
