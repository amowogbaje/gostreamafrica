# ADR-0012: Short-lived JWT access tokens and rotating refresh tokens
Status: Accepted. Supersedes ADR-0011's opaque sessions; its argon2id password rules remain.

## Context
Phase 1 needs sign-in for a Next.js app and, later, a mobile app, through the same endpoints. The earlier design (one opaque session cookie) worked for a browser but gave a mobile client nothing to hold, no cheap way for other modules to authorize requests, and no way to notice a stolen long-lived credential. We also need email verification, password reset, lockout after repeated failures, and roles.

## Decision
- **Access token:** a JWT, HS256, 15 minutes, with `sub`, `role`, `jti`, `iss`, `aud`, `exp`. Only HS256 is accepted. The secret comes from `AUTH_JWT_SECRET` (at least 32 characters; the example value is refused outside `APP_ENV=local`). Requests are authorized from the token alone, with no database call.
- **Refresh token:** 32 random bytes, stored only as a SHA-256 hash, in a family. Every refresh marks the token used and issues the next one; presenting a used token revokes the whole family. 30 day idle lifetime, 90 day absolute cap. A row lock makes simultaneous use of one token resolve to a single winner.
- **Transport:** web gets the refresh token as an `HttpOnly`, `SameSite=Strict` cookie scoped to `/api/v1/auth` (`Secure` outside local) and never in a response body. Mobile sends `X-Auth-Transport: body` and keeps the token in the keystore; the same endpoints accept it in the JSON body. Cookie requests must come from an allowlisted `Origin`.
- **One-time tokens** (email verification 24 h, password reset 1 h): hashed, single use consumed atomically, a newer one voids the older. A reset revokes every refresh token.
- **Login protection:** per-IP rate limit; lockout per (account, IP) after 5 failures with exponential backoff (30 s to 15 min); an account-wide lock after 30 failures from any IPs. Wrong password and unknown email are indistinguishable. All limits fail closed when Redis is down.
- **Authorization:** `Authenticate`, `OptionalAuthenticate` and `RequireRole` middleware; `AssertOwner` and `RequireTitleOwner` for creator-owned titles. A role in a token grants nothing without a verified user id.
- **Email:** SMTP through Mailpit locally; sent in the background, so request time does not reveal whether an account exists.
- The users table keeps `status` (suspended accounts cannot sign in or refresh) and `email_verified_at` (recorded now, enforced later by billing).

## Consequences
- Other modules authorize with `auth.Authenticate` and read `auth.PrincipalFrom`; no per-request database lookup.
- A stolen refresh token is detected the first time the thief or the victim uses it; the cost is that the legitimate user is signed out too.
- An access token cannot be revoked early: suspensions and role changes reach existing tokens within 15 minutes (refresh and login apply them at once).
- The web client must refresh from a single place at a time, because two simultaneous refreshes look like theft.
- A single shared HS256 secret means rotating it signs everyone out; use asymmetric keys with a `kid` if other services need to verify tokens.
- Open items and accepted risks are listed in [security-auth.md](../security-auth.md).
