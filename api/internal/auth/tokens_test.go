package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var testSecret = []byte("0123456789abcdef0123456789abcdef-test")

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newIssuer(t *testing.T, c *clock) *TokenIssuer {
	t.Helper()
	i, err := NewTokenIssuer(testSecret, 15*time.Minute, c.now)
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func TestTokenIssuer_RoundTrip(t *testing.T) {
	c := &clock{t: time.Now()}
	i := newIssuer(t, c)
	raw, exp, err := i.Issue("user-1", RoleCreator)
	if err != nil {
		t.Fatal(err)
	}
	if !exp.Equal(c.t.Add(15 * time.Minute)) {
		t.Fatalf("exp=%v", exp)
	}
	got, err := i.Verify(raw)
	if err != nil || got.UserID != "user-1" || got.Role != RoleCreator || got.TokenID == "" {
		t.Fatalf("claims=%+v err=%v", got, err)
	}
}

func TestTokenIssuer_Expiry(t *testing.T) {
	c := &clock{t: time.Now()}
	i := newIssuer(t, c)
	raw, _, _ := i.Issue("u", RoleViewer)

	c.t = c.t.Add(15*time.Minute + 3*time.Second) // inside the 5s leeway
	if _, err := i.Verify(raw); err != nil {
		t.Fatalf("inside leeway should pass: %v", err)
	}
	c.t = c.t.Add(10 * time.Second)
	if _, err := i.Verify(raw); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("expired token: err=%v", err)
	}
}

// forge builds a token with arbitrary claims, header algorithm and key.
func forge(t *testing.T, method jwt.SigningMethod, key any, claims jwt.MapClaims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(method, claims).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func goodClaims(c *clock) jwt.MapClaims {
	return jwt.MapClaims{
		"iss": jwtIssuer, "aud": jwtAudience, "sub": "user-1", "role": "admin",
		"exp": c.t.Add(time.Hour).Unix(), "nbf": c.t.Add(-time.Minute).Unix(), "iat": c.t.Unix(),
	}
}

func TestTokenIssuer_RejectsForgeries(t *testing.T) {
	c := &clock{t: time.Now()}
	i := newIssuer(t, c)
	valid, _, _ := i.Issue("user-1", RoleViewer)
	parts := strings.Split(valid, ".")

	// payload swapped to claim admin while keeping the original signature
	var payload map[string]any
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	_ = json.Unmarshal(raw, &payload)
	payload["role"] = "admin"
	edited, _ := json.Marshal(payload)
	tamperedPayload := parts[0] + "." + base64.RawURLEncoding.EncodeToString(edited) + "." + parts[2]

	noSig := forge(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, goodClaims(c))
	wrongKey := forge(t, jwt.SigningMethodHS256, []byte("a-completely-different-secret-key-0000"), goodClaims(c))
	hs384 := forge(t, jwt.SigningMethodHS384, testSecret, goodClaims(c))
	hs512 := forge(t, jwt.SigningMethodHS512, testSecret, goodClaims(c))

	badIss := goodClaims(c)
	badIss["iss"] = "someone-else"
	badAud := goodClaims(c)
	badAud["aud"] = "another-api"
	noExp := goodClaims(c)
	delete(noExp, "exp")
	noSub := goodClaims(c)
	delete(noSub, "sub")
	badRole := goodClaims(c)
	badRole["role"] = "superuser"
	notYet := goodClaims(c)
	notYet["nbf"] = c.t.Add(time.Hour).Unix()

	cases := map[string]string{
		"tampered payload (role escalation)": tamperedPayload,
		"truncated signature":                valid[:len(valid)-4],
		"alg none":                           noSig,
		"wrong key":                          wrongKey,
		"HS384 with the right key":           hs384,
		"HS512 with the right key":           hs512,
		"wrong issuer":                       forge(t, jwt.SigningMethodHS256, testSecret, badIss),
		"wrong audience":                     forge(t, jwt.SigningMethodHS256, testSecret, badAud),
		"no expiry":                          forge(t, jwt.SigningMethodHS256, testSecret, noExp),
		"no subject":                         forge(t, jwt.SigningMethodHS256, testSecret, noSub),
		"unknown role":                       forge(t, jwt.SigningMethodHS256, testSecret, badRole),
		"not valid yet":                      forge(t, jwt.SigningMethodHS256, testSecret, notYet),
		"empty":                              "",
		"garbage":                            "not-a-jwt",
		"oversized":                          strings.Repeat("a", maxJWTLen+1),
	}
	for name, tok := range cases {
		if _, err := i.Verify(tok); !errors.Is(err, ErrTokenInvalid) {
			t.Errorf("%s: err=%v, want ErrTokenInvalid", name, err)
		}
	}
	// control: a correctly forged-but-valid token for the same key is accepted
	if _, err := i.Verify(forge(t, jwt.SigningMethodHS256, testSecret, goodClaims(c))); err != nil {
		t.Fatalf("control token rejected: %v", err)
	}
}

func TestNewTokenIssuer_RejectsWeakConfig(t *testing.T) {
	if _, err := NewTokenIssuer([]byte("too-short"), time.Minute, time.Now); err == nil {
		t.Error("short secret accepted")
	}
	if _, err := NewTokenIssuer(testSecret, 0, time.Now); err == nil {
		t.Error("zero ttl accepted")
	}
}
