# Authentication threat model

Scope: registration, login, tokens, email verification, password reset, roles and ownership (`api/internal/auth`). The design is in [ADR-0012](adr/0012-jwt-access-and-rotating-refresh-tokens.md). "Tested" means a named test in `api/internal/auth` fails if the defence is removed (each was confirmed by deliberately breaking the code).

## What we protect and who we worry about

| Asset | Why it matters |
|---|---|
| Passwords | Reused on other sites; a leak harms users beyond us. |
| Refresh tokens | Long lived: whoever holds one is the user for up to 90 days. |
| Access tokens | Short lived (15 min), but grant the user's role. |
| Email verification and reset links | Whoever opens one controls the account. |
| Roles | `creator` and `admin` can publish and manage content. |

Attackers considered: someone guessing or stuffing passwords from the internet; someone who steals a token (XSS, a leaked log, a stolen device); someone on another website making the victim's browser send requests; a user trying to give themselves more rights; someone reading our database after a breach.

## Attacks the design addresses

| Attack | Defence | Tested by |
|---|---|---|
| Password guessing and credential stuffing | argon2id (19 MiB, t=2); per-IP limit (20 per 10 min); lockout per account and IP after 5 failures with exponential backoff (30 s up to 15 min); account-wide lock after 30 failures from any IPs; a blocklist of common passwords; 10 to 128 character length rule | `TestLogin_LockoutWiring`, `TestRedisTracker_*`, `TestCheckPassword`, `TestRateLimits` |
| Finding which emails have accounts (login) | Wrong password and unknown email return the same 401; the unknown path still runs a hash so timing matches; both count toward lockout | `TestLogin_FailuresAreIndistinguishable` |
| Finding accounts via forgot or resend | Always `202` with the same text; per-account and per-IP limits applied silently | `TestForgotAndResetPassword`, `TestResendVerification_NeverRevealsAccounts` |
| Database leak | Passwords are argon2id (PHC string, parameters upgradeable on login). Refresh and one-time tokens are stored as SHA-256 only, so a leaked table cannot be replayed | `TestIntegration_RegisterVerifyLoginRefreshLogout` |
| Stolen refresh token | Rotation on every use. Presenting an already-used token revokes its whole family (the thief and the victim both lose the session). 30 day idle limit and a 90 day absolute cap | `TestRefresh_RotationAndReuseDetection`, `TestPG_RefreshRotation`, `TestRefresh_ExpiryAndAbsoluteLifetime` |
| Two requests racing on one refresh token | A row lock lets exactly one win; losers are rejected and the family is revoked | `TestPG_ConcurrentRefreshOnlyOneWins`, `TestIntegration_DoubleSubmitOnlyOneSucceeds` |
| XSS reading tokens | The web refresh token is an HttpOnly cookie. A cookie client never receives it in a response body, so script in the page cannot read it, even through the refresh endpoint | `TestRefresh_RotationAndReuseDetection` (body must be empty) |
| CSRF on cookie endpoints | `SameSite=Strict`; the refresh and logout endpoints check `Origin` against an allowlist; all writes need `Content-Type: application/json`; everything else uses a Bearer header that browsers do not attach on their own | `TestRefresh_CookieRequestsAreOriginChecked` |
| Forged or altered JWTs | HS256 only: `alg: none`, HS384 and HS512 are rejected; issuer, audience and expiry are required; an edited payload fails the signature; the secret must be at least 32 characters and the example secret is refused outside `APP_ENV=local` | `TestTokenIssuer_RejectsForgeries`, `TestAuthn_RejectsBadAndForgedTokens`, `TestLoad_NonLocalHardening` |
| Giving yourself a higher role | Registration always creates `viewer`; JSON with unknown fields (`role`, `status`, `email_verified_at`) is rejected; `PATCH /v1/me` cannot touch them; roles come from the database, so promotion needs a database change; a role claim without a verified user id grants nothing in the catalog | `TestRegister_DuplicateAndMassAssignment`, `TestMe_CannotChangeRoleOrIdentity`, `TestAuthz_RolesAndEscalation`, `TestVisibility_ListByRole` |
| Touching someone else's content | `RequireRole`, `AssertOwner`, `AssertTitleOwner` (owner or admin); other people's drafts answer 404 so they do not even confirm existence | `TestOwnershipHelpers`, `TestOwnershipMiddleware`, `TestVisibility_DetailAndTitleType` |
| Replaying or misusing email links | Single use, enforced atomically in SQL (tested with 8 concurrent attempts); 24 h and 1 h expiry; a newer link voids older ones; a verification token cannot reset a password; a weak new password is rejected before the token is consumed | `TestPG_OneTimeTokens`, `TestPG_ResetPassword_SingleUseUnderConcurrency`, `TestEmailTokensAreNotInterchangeable` |
| Resetting a password to take over | A reset revokes every refresh token of the user | `TestForgotAndResetPassword` |
| Email header injection | Recipient and subject are validated; CR or LF is rejected | `TestSMTPMailer_RejectsInjection` |
| Credentials sent in clear over SMTP | The mailer refuses to authenticate without TLS; `SMTP_REQUIRE_TLS` is on outside local | `TestSMTPMailer_NoCredentialsOverPlaintext` |
| SQL injection | Parameterized SQL only; hostile inputs (cursors, search text, ids) are tested | `TestSearch`, `TestParseCursorRejects`, `TestPG_TitleOwnerLookup` |
| Resource exhaustion through hashing or large bodies | At most 4 hashes run at once; password limited to 128 bytes; request bodies limited to 8 KB; cursors and queries have length limits | `TestHasher_ConcurrencyGuardHonoursContext`, `TestRequestShape` |
| Secrets in logs | Tokens and passwords are never logged (checked on a live run); reuse of a refresh token logs a `SECURITY` line with user and family ids only | live run, `TestLoad_MissingAndInvalid` (config errors never echo secrets) |
| Redis or limiter down | Auth endpoints fail closed (503), never unlimited guessing | `TestRateLimiterDown_FailsClosed`, `TestLogin_FailsClosedWhenTrackerDown` |

## Using the same endpoints from a mobile app

Web and mobile call the same endpoints; only how the refresh token travels differs.

1. Register or log in with the header `X-Auth-Transport: body`. The response contains `access_token`, `refresh_token`, `expires_in` and `refresh_expires_in` and sets no cookie.
2. Keep the refresh token in the platform keystore (iOS Keychain, Android Keystore), never in plain preferences. Keep the access token in memory.
3. When the access token expires (or on a 401 with `token_expired`), call `POST /v1/auth/refresh` with `{"refresh_token": "..."}`. The response contains a **new** refresh token: store it and discard the old one immediately.
4. Refresh from one place only (a single in-flight request). Two simultaneous refreshes look like theft and end the session.
5. `POST /v1/auth/logout` with the refresh token in the body ends that device's session; `POST /v1/auth/logout-all` (Bearer) ends all of them.
6. A cookie-based request never receives the refresh token in its body, so the body transport cannot be abused from a web page: it is honoured only when the token arrives in the body.

## What remains open (honest list)

| Risk | Status |
|---|---|
| Registration answers `409 email_taken`, which reveals that an address has an account. | Accepted for now; limited by the 5 per hour per IP register limit. Fix with a "check your email" flow once email verification gates access. |
| An access token cannot be revoked before it expires (15 min). A suspended or demoted user keeps their old rights for up to that long. | Accepted. Lower the TTL or add a denylist if it becomes a problem. Refresh and login already stop immediately. |
| Per-account lockout lets an attacker lock a victim out of one IP; the account-wide lock (30 failures) can lock them out everywhere for 5 to 30 minutes. | Accepted trade-off; the pair key (account and IP) makes the first case harmless to the owner on another network. Add CAPTCHA or email unlock later. |
| Multiple tabs refreshing at the same moment will revoke the family. | The web app must serialise refreshes (single-flight). Needs doing in the Next.js client. |
| One shared HS256 secret; no key id, so rotating it signs everyone out. | Accepted for a single API. Move to asymmetric keys with a `kid` if another service must verify tokens. |
| No multi-factor authentication, no breached-password service (only a 90 entry list), no email change flow, no "your password changed" notification. | Not built. |
| Rate limits and lockout depend on `TRUSTED_PROXY_HOPS` matching the real number of proxies. Too low merges users behind a proxy; too high lets clients forge `X-Forwarded-For`. | Document per environment; the production safety check is not built yet. |
| No CAPTCHA or bot defence on register and login. | Not built. |
| Users with unverified email can sign in; verification does not gate anything yet. | Billing will require it. |
| Refresh cookie `Secure` is off for `APP_ENV=local` (plain http); its `Path` must match the public URL prefix (`/api/v1/auth` behind nginx). | Documented in the env file. |
| Expired rows in `refresh_tokens` and `one_time_tokens` are never deleted. | A cleanup job is not built. |
| No CSP, HSTS or security headers. | Belongs to the nginx and web steps. |
| Redis is a hard dependency of sign-in (fail closed). | Accepted: availability traded for safety. |
