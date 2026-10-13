package auth

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCheckEmail(t *testing.T) {
	good := []string{"ada@example.com", "a.b+tag@sub.example.co.ng", "Ada@Example.COM"}
	for _, s := range good {
		if msg := checkEmail(s); msg != "" {
			t.Errorf("%q rejected: %s", s, msg)
		}
	}
	bad := []string{
		"", "plain", "no@dot", "@example.com", "ada@", "ada@.com", "ada@example.",
		"Ada <ada@example.com>", "ada@example.com, bob@example.com", "ada @example.com",
		strings.Repeat("a", 250) + "@example.com",
	}
	for _, s := range bad {
		if msg := checkEmail(s); msg == "" {
			t.Errorf("%q accepted", s)
		}
	}
}

func TestCheckPassword(t *testing.T) {
	cases := []struct {
		name, pw, email string
		ok              bool
	}{
		{"good", "correct horse battery", "ada@example.com", true},
		{"exactly ten", "xK7mQ2vL9z", "", true},
		{"nine characters", "012345678", "", false},
		{"empty", "", "", false},
		{"common", "password123", "", false},
		{"common any case", "PassWord1234", "", false},
		{"common keyboard", "qwertyuiop", "", false},
		{"common digits", "0123456789", "", false},
		{"app name", "StreamAfrica123", "", false},
		{"same as email", "ada@example.com", "ada@example.com", false},
		{"same as local part", "adalovelace", "adalovelace@example.com", false},
		{"128 bytes", strings.Repeat("a", 128), "", true},
		{"129 bytes", strings.Repeat("a", 129), "", false},
		{"unicode counts runes", "ẹ̀bùn àdùn ọmọ", "", true},
		{"ten emoji", strings.Repeat("🔑", 10), "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if msg := CheckPassword(c.pw, c.email); (msg == "") != c.ok {
				t.Fatalf("CheckPassword(%q) = %q, want ok=%v", c.pw, msg, c.ok)
			}
		})
	}
}

func TestRegisterValidate(t *testing.T) {
	cases := []struct {
		name  string
		req   registerRequest
		field string // empty means valid
	}{
		{"valid", registerRequest{"ada@example.com", "correct horse battery", "Ada"}, ""},
		{"bad email", registerRequest{"nope", "correct horse battery", "Ada"}, "email"},
		{"short password", registerRequest{"ada@example.com", "short", "Ada"}, "password"},
		{"common password", registerRequest{"ada@example.com", "iloveyou123", "Ada"}, "password"},
		{"empty name", registerRequest{"ada@example.com", "correct horse battery", ""}, "display_name"},
		{"long name", registerRequest{"ada@example.com", "correct horse battery", strings.Repeat("n", 81)}, "display_name"},
		{"control char in name", registerRequest{"ada@example.com", "correct horse battery", "Ada\x00"}, "display_name"},
		{"unicode name", registerRequest{"ada@example.com", "correct horse battery", "Ọláolú Adéyẹmí"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := c.req.validate()
			if c.field == "" && len(f) != 0 {
				t.Fatalf("valid request rejected: %v", f)
			}
			if c.field != "" && f[c.field] == "" {
				t.Fatalf("%s not flagged: %v", c.field, f)
			}
		})
	}
}

func TestLoginValidate(t *testing.T) {
	ok := loginRequest{Email: "anything", Password: "x"}
	if f := ok.validate(); len(f) != 0 {
		t.Fatalf("login must not judge email format or password rules: %v", f)
	}
	if f := (&loginRequest{}).validate(); f["email"] == "" || f["password"] == "" {
		t.Fatalf("empty login accepted: %v", f)
	}
}

func TestResetPasswordValidate(t *testing.T) {
	if f := (&resetPasswordRequest{Token: "t", NewPassword: "correct horse battery"}).validate(); len(f) != 0 {
		t.Fatalf("valid rejected: %v", f)
	}
	if f := (&resetPasswordRequest{Token: "t", NewPassword: "password123"}).validate(); f["new_password"] == "" {
		t.Fatal("common password accepted")
	}
	if f := (&resetPasswordRequest{NewPassword: "correct horse battery"}).validate(); f["token"] == "" {
		t.Fatal("missing token accepted")
	}
}

func TestPatchMeValidate(t *testing.T) {
	parse := func(body string) *patchMeRequest {
		var r patchMeRequest
		if err := json.Unmarshal([]byte(body), &r); err != nil {
			t.Fatal(err)
		}
		r.normalize()
		return &r
	}
	cases := []struct {
		name, body, field string // field empty means valid
	}{
		{"empty patch", `{}`, "body"},
		{"display name", `{"display_name":" Ada "}`, ""},
		{"null display name", `{"display_name":null}`, "display_name"},
		{"blank display name", `{"display_name":"   "}`, "display_name"},
		{"clear avatar", `{"avatar_url":null}`, ""},
		{"https avatar", `{"avatar_url":"https://cdn.example.com/a.png"}`, ""},
		{"http avatar", `{"avatar_url":"http://cdn.example.com/a.png"}`, "avatar_url"},
		{"javascript avatar", `{"avatar_url":"javascript:alert(1)"}`, "avatar_url"},
		{"avatar with space", `{"avatar_url":"https://x.example.com/a b.png"}`, "avatar_url"},
		{"country ok", `{"country":"NG"}`, ""},
		{"country lowercase", `{"country":"ng"}`, "country"},
		{"country long", `{"country":"NGA"}`, "country"},
		{"clear country", `{"country":null}`, ""},
		{"language ok", `{"preferred_language":"pcm"}`, ""},
		{"language region", `{"preferred_language":"en-NG"}`, ""},
		{"language bad", `{"preferred_language":"English"}`, "preferred_language"},
		{"null language", `{"preferred_language":null}`, "preferred_language"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := parse(c.body).validate()
			if c.field == "" && len(f) != 0 {
				t.Fatalf("valid patch rejected: %v", f)
			}
			if c.field != "" && f[c.field] == "" {
				t.Fatalf("%s not flagged: %v", c.field, f)
			}
		})
	}
	p := parse(`{"display_name":" Ada ","country":null}`).toPatch()
	if p.DisplayName == nil || *p.DisplayName != "Ada" || !p.CountrySet || p.Country != nil || p.AvatarURLSet {
		t.Fatalf("toPatch: %+v", p)
	}
}
