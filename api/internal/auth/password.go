package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// ErrInvalidHash means a stored password hash could not be parsed.
var ErrInvalidHash = errors.New("invalid password hash")

// Params are the argon2id cost settings.
type Params struct {
	MemoryKiB     uint32
	Time          uint32
	Parallelism   uint8
	SaltLen       uint32 // default 16
	KeyLen        uint32 // default 32
	MaxConcurrent int    // simultaneous hashes allowed (memory guard), default 4
}

// Hasher hashes and verifies passwords with argon2id. The encoded form is the
// standard PHC string, so parameters can be raised later and old hashes still verify.
type Hasher struct {
	p   Params
	sem chan struct{}
}

// NewHasher validates params and fills defaults.
func NewHasher(p Params) (*Hasher, error) {
	if p.SaltLen == 0 {
		p.SaltLen = 16
	}
	if p.KeyLen == 0 {
		p.KeyLen = 32
	}
	if p.MaxConcurrent <= 0 {
		p.MaxConcurrent = 4
	}
	if p.Parallelism < 1 || p.Time < 1 || p.MemoryKiB < 8*uint32(p.Parallelism) {
		return nil, errors.New("invalid argon2 parameters")
	}
	return &Hasher{p: p, sem: make(chan struct{}, p.MaxConcurrent)}, nil
}

func (h *Hasher) acquire(ctx context.Context) error {
	select {
	case h.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Hasher) release() { <-h.sem }

// Hash returns the encoded argon2id hash of password with a fresh random salt.
func (h *Hasher) Hash(ctx context.Context, password string) (string, error) {
	if err := h.acquire(ctx); err != nil {
		return "", err
	}
	defer h.release()

	salt := make([]byte, h.p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("read salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, h.p.Time, h.p.MemoryKiB, h.p.Parallelism, h.p.KeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, h.p.MemoryKiB, h.p.Time, h.p.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// Verify checks password against encoded in constant time. needsRehash is true
// when the hash is valid but was made with weaker parameters than the current ones.
func (h *Hasher) Verify(ctx context.Context, password, encoded string) (ok, needsRehash bool, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false, false, ErrInvalidHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, false, ErrInvalidHash
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false, false, ErrInvalidHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return false, false, ErrInvalidHash
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 || p < 1 || t < 1 || m < 8*uint32(p) {
		return false, false, ErrInvalidHash
	}

	if err := h.acquire(ctx); err != nil {
		return false, false, err
	}
	defer h.release()

	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return false, false, nil
	}
	weaker := m != h.p.MemoryKiB || t != h.p.Time || p != h.p.Parallelism
	return true, weaker, nil
}
