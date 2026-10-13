# ADR-0011: Server-side sessions and argon2id passwords
Status: Superseded by [ADR-0012](0012-jwt-access-and-rotating-refresh-tokens.md) (the argon2id password rules still apply)

## Context
Phase 1 needs registration and login for a browser app served from the same origin as the API (nginx routes `/api`). Tokens must survive a database leak, passwords must be expensive to crack, and login must resist guessing without making accounts easy to enumerate.

## Decision
- **Passwords:** argon2id (`golang.org/x/crypto/argon2`), 16-byte random salt, defaults m=19456 KiB, t=2, p=1 (OWASP minimum), stored as a PHC string so parameters can be raised later. A login with an older hash upgrades it. At most 4 hashes run at once, to cap memory on a small VPS. Passwords: 10 to 128 bytes, no composition rules.
- **Sessions:** opaque 32-byte random tokens in an `HttpOnly`, `SameSite=Lax` cookie (`Secure` outside `APP_ENV=local`). The database stores only the SHA-256 of the token, with a fixed 30 day expiry. No JWTs, so logout and revocation take effect immediately.
- **CSRF:** every write endpoint requires `Content-Type: application/json`, which a cross-site HTML form cannot send, plus `SameSite=Lax`.
- **Enumeration:** wrong password and unknown email return the same 401, and the unknown-email path still performs a hash. Registration returns 409 for a taken email. That reveals existence, and is accepted until email verification exists.
- **Rate limits (Redis, fail-closed):** register 5 per hour per IP, login 20 per 10 minutes per IP and 10 per 15 minutes per account.
- **Roles:** `viewer` (default), `creator`, `admin`. Registration cannot choose a role. Unknown JSON fields are rejected.

## Consequences
- A leaked `sessions` table cannot be replayed, and sessions can be revoked by deleting rows.
- Each authenticated request costs one indexed query. Redis could cache this later if it shows up in measurements.
- The per-account limit lets an attacker lock a victim out for up to 15 minutes. Accepted for the MVP, revisit with CAPTCHA or email-based unlock.
- Not built yet: email verification, password reset, "log out everywhere", expired-session cleanup job, and a production check that argon2 settings are not weakened.
