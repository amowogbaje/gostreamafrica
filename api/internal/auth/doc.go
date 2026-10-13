// Package auth owns accounts, authentication and authorization.
//
// Access tokens are short-lived JWTs (HS256). Refresh tokens are opaque, rotate on every use
// and are stored only as SHA-256 hashes; presenting an already-used refresh token revokes its
// whole family. Passwords use argon2id. See docs/security-auth.md for the threat model.
package auth
