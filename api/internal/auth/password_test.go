package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func testHasher(t *testing.T, memory, iterations uint32) *Hasher {
	t.Helper()
	h, err := NewHasher(Params{MemoryKiB: memory, Time: iterations, Parallelism: 1})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestHasher_RoundTrip(t *testing.T) {
	h := testHasher(t, 64, 1)
	ctx := context.Background()
	enc, err := h.Hash(ctx, "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, "$argon2id$v=19$m=64,t=1,p=1$") {
		t.Fatalf("unexpected format: %s", enc)
	}
	ok, rehash, err := h.Verify(ctx, "correct horse battery", enc)
	if err != nil || !ok || rehash {
		t.Fatalf("ok=%v rehash=%v err=%v", ok, rehash, err)
	}
	ok, _, err = h.Verify(ctx, "wrong password", enc)
	if err != nil || ok {
		t.Fatalf("wrong password accepted: ok=%v err=%v", ok, err)
	}
}

func TestHasher_UniqueSalts(t *testing.T) {
	h := testHasher(t, 64, 1)
	a, _ := h.Hash(context.Background(), "same password")
	b, _ := h.Hash(context.Background(), "same password")
	if a == b {
		t.Fatal("two hashes of the same password must differ")
	}
}

func TestHasher_NeedsRehash(t *testing.T) {
	weak := testHasher(t, 64, 1)
	strong := testHasher(t, 128, 2)
	enc, _ := weak.Hash(context.Background(), "pw")
	ok, rehash, err := strong.Verify(context.Background(), "pw", enc)
	if err != nil || !ok || !rehash {
		t.Fatalf("ok=%v rehash=%v err=%v", ok, rehash, err)
	}
}

func TestHasher_InvalidHashes(t *testing.T) {
	h := testHasher(t, 64, 1)
	bad := []string{
		"",
		"plaintext",
		"$argon2i$v=19$m=64,t=1,p=1$c2FsdHNhbHQ$aGFzaA",
		"$argon2id$v=18$m=64,t=1,p=1$c2FsdHNhbHQ$aGFzaA",
		"$argon2id$v=19$m=64,t=1$c2FsdHNhbHQ$aGFzaA",
		"$argon2id$v=19$m=64,t=1,p=1$!!!$aGFzaA",
		"$argon2id$v=19$m=64,t=1,p=1$c2FsdHNhbHQ$",
		"$argon2id$v=19$m=1,t=1,p=1$c2FsdHNhbHQ$aGFzaA",
	}
	for _, enc := range bad {
		if _, _, err := h.Verify(context.Background(), "pw", enc); !errors.Is(err, ErrInvalidHash) {
			t.Errorf("%q: got err=%v want ErrInvalidHash", enc, err)
		}
	}
}

func TestNewHasher_RejectsBadParams(t *testing.T) {
	for _, p := range []Params{
		{MemoryKiB: 64, Time: 0, Parallelism: 1},
		{MemoryKiB: 64, Time: 1, Parallelism: 0},
		{MemoryKiB: 4, Time: 1, Parallelism: 1},
	} {
		if _, err := NewHasher(p); err == nil {
			t.Errorf("expected error for %+v", p)
		}
	}
}

func TestHasher_ConcurrencyGuardHonoursContext(t *testing.T) {
	h, err := NewHasher(Params{MemoryKiB: 64, Time: 1, Parallelism: 1, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	h.sem <- struct{}{} // pretend another hash is running
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := h.Hash(ctx, "pw"); err == nil {
		t.Fatal("expected context error while the guard is full")
	}
}
