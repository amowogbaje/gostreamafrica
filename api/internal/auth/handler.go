package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"streamafrica/api/internal/platform/httpx"
	"streamafrica/api/internal/platform/ratelimit"
)

const (
	refreshCookieName = "sa_refresh"
	transportHeader   = "X-Auth-Transport" // "body" asks for the refresh token in the JSON body (mobile)
	maxBodyBytes      = 8 << 10
	maxUALen          = 200
)

// Rate limits. Fixed windows, fail-closed (Redis down means 503, never unlimited guesses).
// Account-scoped rules for resend and forgot are applied silently so they reveal nothing.
var (
	registerRule   = ratelimit.Rule{Name: "register-ip", Limit: 5, Window: time.Hour}
	loginRule      = ratelimit.Rule{Name: "login-ip", Limit: 20, Window: 10 * time.Minute}
	refreshRule    = ratelimit.Rule{Name: "refresh-ip", Limit: 60, Window: 10 * time.Minute}
	verifyRule     = ratelimit.Rule{Name: "verify-ip", Limit: 20, Window: time.Hour}
	resendIPRule   = ratelimit.Rule{Name: "resend-ip", Limit: 5, Window: time.Hour}
	resendAcctRule = ratelimit.Rule{Name: "resend-acct", Limit: 3, Window: time.Hour}
	forgotIPRule   = ratelimit.Rule{Name: "forgot-ip", Limit: 5, Window: time.Hour}
	forgotAcctRule = ratelimit.Rule{Name: "forgot-acct", Limit: 3, Window: time.Hour}
	resetRule      = ratelimit.Rule{Name: "reset-ip", Limit: 10, Window: time.Hour}
)

// HandlerConfig holds the HTTP-level settings.
type HandlerConfig struct {
	CookieSecure     bool
	CookiePath       string   // path the refresh cookie is sent to, as the BROWSER sees it (behind nginx: /api/v1/auth)
	TrustedProxyHops int      // reverse proxies in front of the API
	AllowedOrigins   []string // browser origins allowed to use the refresh cookie
}

// Handler exposes the auth HTTP endpoints.
type Handler struct {
	svc     *Service
	tokens  *TokenIssuer
	limiter ratelimit.Store
	logger  *slog.Logger
	cfg     HandlerConfig
	origins map[string]struct{}
}

// NewHandler builds the handler.
func NewHandler(svc *Service, tokens *TokenIssuer, limiter ratelimit.Store, logger *slog.Logger, cfg HandlerConfig) *Handler {
	if cfg.CookiePath == "" {
		cfg.CookiePath = "/"
	}
	origins := make(map[string]struct{}, len(cfg.AllowedOrigins))
	for _, o := range cfg.AllowedOrigins {
		origins[strings.TrimRight(o, "/")] = struct{}{}
	}
	return &Handler{svc: svc, tokens: tokens, limiter: limiter, logger: logger, cfg: cfg, origins: origins}
}

// Authenticate returns the access-token middleware for other modules to use.
func (h *Handler) Authenticate() func(http.Handler) http.Handler { return Authenticate(h.tokens) }

// OptionalAuthenticate returns middleware for public endpoints that behave differently for
// signed-in users (a missing token is fine; a present but bad token is a 401).
func (h *Handler) OptionalAuthenticate() func(http.Handler) http.Handler {
	return OptionalAuthenticate(h.tokens)
}

// Mount registers the auth routes on mux.
func (h *Handler) Mount(mux httpx.Mux) {
	ipKey := ratelimit.IPKey(h.cfg.TrustedProxyHops)
	limit := func(rule ratelimit.Rule) func(http.Handler) http.Handler {
		return ratelimit.Middleware(h.limiter, h.logger, rule, ipKey)
	}
	authn := Authenticate(h.tokens)
	route := func(path string, handler http.Handler, methods ...string) {
		httpx.Route(mux, path, noStore(handler), methods...)
	}
	fn := func(f http.HandlerFunc) http.Handler { return f }

	route("/v1/auth/register", limit(registerRule)(fn(h.register)), http.MethodPost)
	route("/v1/auth/verify-email", limit(verifyRule)(fn(h.verifyEmail)), http.MethodPost)
	route("/v1/auth/resend-verification", limit(resendIPRule)(fn(h.resendVerification)), http.MethodPost)
	route("/v1/auth/login", limit(loginRule)(fn(h.login)), http.MethodPost)
	route("/v1/auth/refresh", limit(refreshRule)(fn(h.refresh)), http.MethodPost)
	route("/v1/auth/logout", fn(h.logout), http.MethodPost)
	route("/v1/auth/logout-all", authn(fn(h.logoutAll)), http.MethodPost)
	route("/v1/auth/forgot-password", limit(forgotIPRule)(fn(h.forgotPassword)), http.MethodPost)
	route("/v1/auth/reset-password", limit(resetRule)(fn(h.resetPassword)), http.MethodPost)
	route("/v1/me", authn(fn(h.me)), http.MethodGet, http.MethodPatch)
}

// authResponse is the body of register, login and refresh. RefreshToken appears only for
// body-transport clients (mobile); the web app receives it as an HttpOnly cookie instead.
type authResponse struct {
	AccessToken      string  `json:"access_token"`
	TokenType        string  `json:"token_type"`
	ExpiresIn        int     `json:"expires_in"`
	RefreshToken     string  `json:"refresh_token,omitempty"`
	RefreshExpiresIn int     `json:"refresh_expires_in,omitempty"`
	User             User    `json:"user"`
	Profile          Profile `json:"profile"`
}

func (h *Handler) respondAuth(w http.ResponseWriter, status int, res AuthResult, bodyTransport bool) {
	resp := authResponse{
		AccessToken: res.Tokens.AccessToken,
		TokenType:   "Bearer",
		ExpiresIn:   int(h.tokens.TTL().Seconds()),
		User:        res.Me.User,
		Profile:     res.Me.Profile,
	}
	if bodyTransport {
		resp.RefreshToken = res.Tokens.RefreshToken
		resp.RefreshExpiresIn = int(math.Max(1, time.Until(res.Tokens.RefreshExpiresAt).Seconds()))
	} else {
		h.setRefreshCookie(w, res.Tokens.RefreshToken, res.Tokens.RefreshExpiresAt)
	}
	httpx.WriteJSON(w, status, resp)
}

func wantsBodyTransport(r *http.Request) bool {
	return strings.EqualFold(strings.TrimSpace(r.Header.Get(transportHeader)), "body")
}

func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if !httpx.DecodeJSON(w, r, &req, maxBodyBytes) {
		return
	}
	req.normalize()
	if fields := req.validate(); len(fields) > 0 {
		httpx.WriteValidationError(w, r, fields)
		return
	}
	res, err := h.svc.Register(r.Context(), req.Email, req.Password, req.DisplayName, h.meta(r))
	switch {
	case errors.Is(err, ErrEmailTaken):
		httpx.WriteError(w, r, http.StatusConflict, "email_taken", "an account with this email already exists")
	case err != nil:
		h.internalError(w, r, "register", err)
	default:
		h.respondAuth(w, http.StatusCreated, res, wantsBodyTransport(r))
	}
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !httpx.DecodeJSON(w, r, &req, maxBodyBytes) {
		return
	}
	req.normalize()
	if fields := req.validate(); len(fields) > 0 {
		httpx.WriteValidationError(w, r, fields)
		return
	}
	res, err := h.svc.Login(r.Context(), req.Email, req.Password, h.meta(r))
	var locked *LockedError
	switch {
	case errors.Is(err, ErrInvalidCredentials):
		httpx.WriteError(w, r, http.StatusUnauthorized, "invalid_credentials", "invalid email or password")
	case errors.As(err, &locked):
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(locked.RetryAfter.Seconds()))))
		httpx.WriteError(w, r, http.StatusTooManyRequests, "too_many_attempts", "too many failed attempts, try again later")
	case errors.Is(err, ErrAccountDisabled):
		httpx.WriteError(w, r, http.StatusForbidden, "account_disabled", "this account is disabled")
	case errors.Is(err, ErrTrackerUnavailable):
		h.logger.ErrorContext(r.Context(), "lockout store error", "error", err.Error())
		ratelimit.RejectUnavailable(w, r)
	case err != nil:
		h.internalError(w, r, "login", err)
	default:
		h.respondAuth(w, http.StatusOK, res, wantsBodyTransport(r))
	}
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	token, fromCookie, ok := h.refreshTokenFrom(w, r)
	if !ok {
		return
	}
	if fromCookie && !h.originAllowed(r) {
		httpx.WriteError(w, r, http.StatusForbidden, "forbidden_origin", "request origin is not allowed")
		return
	}
	res, err := h.svc.Refresh(r.Context(), token, h.meta(r))
	switch {
	case errors.Is(err, ErrRefreshInvalid):
		if fromCookie {
			h.clearRefreshCookie(w)
		}
		httpx.WriteError(w, r, http.StatusUnauthorized, "invalid_refresh_token", "refresh token is invalid or expired")
	case err != nil:
		h.internalError(w, r, "refresh", err)
	default:
		// A cookie client never gets the refresh token in the body, so script running in the
		// page (XSS) cannot read it. Only a client that sent the token in the body gets one back.
		h.respondAuth(w, http.StatusOK, res, !fromCookie)
	}
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	token, fromCookie, ok := h.refreshTokenFrom(w, r)
	if !ok {
		return
	}
	if fromCookie && !h.originAllowed(r) {
		httpx.WriteError(w, r, http.StatusForbidden, "forbidden_origin", "request origin is not allowed")
		return
	}
	if token != "" {
		if err := h.svc.Logout(r.Context(), token); err != nil {
			h.internalError(w, r, "logout", err)
			return
		}
	}
	h.clearRefreshCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) logoutAll(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFrom(r.Context())
	if err := h.svc.LogoutAll(r.Context(), p.UserID); err != nil {
		h.internalError(w, r, "logout-all", err)
		return
	}
	h.clearRefreshCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) verifyEmail(w http.ResponseWriter, r *http.Request) {
	var req verifyEmailRequest
	if !httpx.DecodeJSON(w, r, &req, maxBodyBytes) {
		return
	}
	switch err := h.svc.VerifyEmail(r.Context(), strings.TrimSpace(req.Token)); {
	case errors.Is(err, ErrInvalidToken):
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_or_expired_token", "this link is invalid or has expired")
	case err != nil:
		h.internalError(w, r, "verify-email", err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h *Handler) resendVerification(w http.ResponseWriter, r *http.Request) {
	h.silentEmailFlow(w, r, resendAcctRule, h.svc.ResendVerification, "resend-verification",
		"If an account exists for that email and is not yet verified, we have sent a new link.")
}

func (h *Handler) forgotPassword(w http.ResponseWriter, r *http.Request) {
	h.silentEmailFlow(w, r, forgotAcctRule, h.svc.ForgotPassword, "forgot-password",
		"If an account exists for that email, we have sent instructions to reset the password.")
}

// silentEmailFlow answers 202 with the same message whether or not the account exists, and
// applies a per-account limit silently (over the limit: same answer, no email), so neither
// the response nor its status can be used to find accounts or to spam someone's inbox.
func (h *Handler) silentEmailFlow(w http.ResponseWriter, r *http.Request, acctRule ratelimit.Rule,
	do func(context.Context, string) error, op, message string) {
	var req emailRequest
	if !httpx.DecodeJSON(w, r, &req, maxBodyBytes) {
		return
	}
	req.normalize()
	if fields := req.validate(); len(fields) > 0 {
		httpx.WriteValidationError(w, r, fields)
		return
	}
	d, err := ratelimit.Allow(r.Context(), h.limiter, acctRule, accountKey(req.Email))
	if err != nil {
		h.logger.ErrorContext(r.Context(), "rate limiter store error", "rule", acctRule.Name, "error", err.Error())
		ratelimit.RejectUnavailable(w, r)
		return
	}
	if d.Allowed {
		if err := do(r.Context(), req.Email); err != nil {
			h.internalError(w, r, op, err)
			return
		}
	}
	httpx.WriteJSON(w, http.StatusAccepted, map[string]string{"message": message})
}

func (h *Handler) resetPassword(w http.ResponseWriter, r *http.Request) {
	var req resetPasswordRequest
	if !httpx.DecodeJSON(w, r, &req, maxBodyBytes) {
		return
	}
	req.Token = strings.TrimSpace(req.Token)
	if fields := req.validate(); len(fields) > 0 {
		httpx.WriteValidationError(w, r, fields) // validated BEFORE the token is consumed
		return
	}
	switch err := h.svc.ResetPassword(r.Context(), req.Token, req.NewPassword); {
	case errors.Is(err, ErrInvalidToken):
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_or_expired_token", "this link is invalid or has expired")
	case err != nil:
		h.internalError(w, r, "reset-password", err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFrom(r.Context())
	if r.Method == http.MethodPatch {
		var req patchMeRequest
		if !httpx.DecodeJSON(w, r, &req, maxBodyBytes) {
			return
		}
		req.normalize()
		if fields := req.validate(); len(fields) > 0 {
			httpx.WriteValidationError(w, r, fields)
			return
		}
		me, err := h.svc.UpdateProfile(r.Context(), p.UserID, req.toPatch())
		h.writeMe(w, r, me, err, "update-profile")
		return
	}
	me, err := h.svc.Me(r.Context(), p.UserID)
	h.writeMe(w, r, me, err, "me")
}

func (h *Handler) writeMe(w http.ResponseWriter, r *http.Request, me Me, err error, op string) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, r, http.StatusUnauthorized, "unauthenticated", "authentication required")
	case err != nil:
		h.internalError(w, r, op, err)
	default:
		httpx.WriteJSON(w, http.StatusOK, me)
	}
}

// refreshTokenFrom reads the refresh token from the HttpOnly cookie (web) or, when there is no
// cookie, from a JSON body {"refresh_token": "..."} (mobile). It writes the error itself if the body is bad.
func (h *Handler) refreshTokenFrom(w http.ResponseWriter, r *http.Request) (token string, fromCookie, ok bool) {
	if c, err := r.Cookie(refreshCookieName); err == nil && c.Value != "" {
		return c.Value, true, true
	}
	if r.ContentLength == 0 {
		return "", false, true
	}
	var b refreshBody
	if !httpx.DecodeJSON(w, r, &b, maxBodyBytes) {
		return "", false, false
	}
	return b.RefreshToken, false, true
}

// originAllowed protects the cookie-authenticated endpoints from cross-site requests. Browsers
// always send Origin on cross-site POSTs; non-browser clients usually send none and use the body transport.
func (h *Handler) originAllowed(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	_, ok := h.origins[strings.TrimRight(o, "/")]
	return ok
}

func (h *Handler) setRefreshCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: refreshCookieName, Value: token, Path: h.cfg.CookiePath, Expires: expires,
		HttpOnly: true, Secure: h.cfg.CookieSecure, SameSite: http.SameSiteStrictMode,
	})
}

func (h *Handler) clearRefreshCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: refreshCookieName, Value: "", Path: h.cfg.CookiePath, MaxAge: -1,
		HttpOnly: true, Secure: h.cfg.CookieSecure, SameSite: http.SameSiteStrictMode,
	})
}

func (h *Handler) meta(r *http.Request) ClientMeta {
	ua := r.UserAgent()
	if len(ua) > maxUALen {
		ua = ua[:maxUALen]
	}
	return ClientMeta{UserAgent: ua, IP: ratelimit.ClientIP(r, h.cfg.TrustedProxyHops)}
}

func (h *Handler) internalError(w http.ResponseWriter, r *http.Request, op string, err error) {
	h.logger.ErrorContext(r.Context(), "auth operation failed",
		"op", op, "request_id", httpx.RequestIDFrom(r.Context()), "error", err.Error())
	httpx.WriteError(w, r, http.StatusInternalServerError, "internal_error", "internal server error")
}

func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// accountKey identifies an account in rate-limit keys without storing the email itself.
func accountKey(email string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(email)))
	return hex.EncodeToString(sum[:])
}
