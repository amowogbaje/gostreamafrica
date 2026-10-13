package auth

import (
	"encoding/json"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	minPasswordRunes = 10
	maxPasswordBytes = 128 // caps argon2 work per request
	maxNameRunes     = 80
	maxEmailLen      = 254
	maxAvatarURLLen  = 500
)

var (
	countryRE  = regexp.MustCompile(`^[A-Z]{2}$`)
	languageRE = regexp.MustCompile(`^[a-z]{2,3}(-[A-Z]{2})?$`)
)

// optional distinguishes "field absent" (Set false) from "field present"; an explicit
// JSON null gives Set true and a nil Value. It lets PATCH clear a field.
type optional[T any] struct {
	Set   bool
	Value *T
}

func (o *optional[T]) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Value = nil
		return nil
	}
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	o.Value = &v
	return nil
}

type registerRequest struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type emailRequest struct {
	Email string `json:"email"`
}

type verifyEmailRequest struct {
	Token string `json:"token"`
}

type resetPasswordRequest struct {
	Token       string `json:"token"`
	NewPassword string `json:"new_password"`
}

type refreshBody struct {
	RefreshToken string `json:"refresh_token"`
}

type patchMeRequest struct {
	DisplayName       optional[string] `json:"display_name"`
	AvatarURL         optional[string] `json:"avatar_url"`
	Country           optional[string] `json:"country"`
	PreferredLanguage optional[string] `json:"preferred_language"`
}

func (r *registerRequest) normalize() {
	r.Email = strings.TrimSpace(r.Email)
	r.DisplayName = strings.TrimSpace(r.DisplayName)
}

func (r *registerRequest) validate() map[string]string {
	f := map[string]string{}
	if msg := checkEmail(r.Email); msg != "" {
		f["email"] = msg
	}
	if msg := CheckPassword(r.Password, r.Email); msg != "" {
		f["password"] = msg
	}
	if msg := checkDisplayName(r.DisplayName); msg != "" {
		f["display_name"] = msg
	}
	return f
}

func (r *loginRequest) normalize() { r.Email = strings.TrimSpace(r.Email) }

// validate only checks presence and size. It never judges the email format or the
// password rules, so login cannot be used to learn anything about accounts.
func (r *loginRequest) validate() map[string]string {
	f := map[string]string{}
	if r.Email == "" || len(r.Email) > maxEmailLen {
		f["email"] = "is required"
	}
	if r.Password == "" || len(r.Password) > maxPasswordBytes {
		f["password"] = "is required"
	}
	return f
}

func (r *emailRequest) normalize() { r.Email = strings.TrimSpace(r.Email) }

func (r *emailRequest) validate() map[string]string {
	if r.Email == "" || len(r.Email) > maxEmailLen {
		return map[string]string{"email": "is required"}
	}
	return map[string]string{}
}

func (r *resetPasswordRequest) validate() map[string]string {
	f := map[string]string{}
	if r.Token == "" || len(r.Token) > 128 {
		f["token"] = "is required"
	}
	if msg := CheckPassword(r.NewPassword, ""); msg != "" {
		f["new_password"] = msg
	}
	return f
}

func (r *patchMeRequest) normalize() {
	if r.DisplayName.Value != nil {
		v := strings.TrimSpace(*r.DisplayName.Value)
		r.DisplayName.Value = &v
	}
}

func (r *patchMeRequest) validate() map[string]string {
	f := map[string]string{}
	if !r.DisplayName.Set && !r.AvatarURL.Set && !r.Country.Set && !r.PreferredLanguage.Set {
		f["body"] = "must contain at least one field to update"
		return f
	}
	if r.DisplayName.Set {
		if r.DisplayName.Value == nil {
			f["display_name"] = "cannot be null"
		} else if msg := checkDisplayName(*r.DisplayName.Value); msg != "" {
			f["display_name"] = msg
		}
	}
	if r.AvatarURL.Set && r.AvatarURL.Value != nil {
		if msg := checkAvatarURL(*r.AvatarURL.Value); msg != "" {
			f["avatar_url"] = msg
		}
	}
	if r.Country.Set && r.Country.Value != nil && !countryRE.MatchString(*r.Country.Value) {
		f["country"] = "must be a two-letter uppercase country code such as NG"
	}
	if r.PreferredLanguage.Set {
		if r.PreferredLanguage.Value == nil {
			f["preferred_language"] = "cannot be null"
		} else if !languageRE.MatchString(*r.PreferredLanguage.Value) {
			f["preferred_language"] = "must be a language code such as en, yo, ig, ha or pcm"
		}
	}
	return f
}

func (r *patchMeRequest) toPatch() ProfilePatch {
	p := ProfilePatch{DisplayName: r.DisplayName.Value, PreferredLanguage: r.PreferredLanguage.Value}
	if r.AvatarURL.Set {
		p.AvatarURLSet, p.AvatarURL = true, r.AvatarURL.Value
	}
	if r.Country.Set {
		p.CountrySet, p.Country = true, r.Country.Value
	}
	return p
}

// CheckPassword applies the password policy and returns a message, or "" when acceptable.
// email may be empty; when set the password must not be the email or its local part.
func CheckPassword(pw, email string) string {
	if utf8.RuneCountInString(pw) < minPasswordRunes {
		return "must be at least 10 characters"
	}
	if len(pw) > maxPasswordBytes {
		return "must be at most 128 bytes"
	}
	if isCommonPassword(pw) {
		return "is too common, choose a less guessable password"
	}
	if email != "" {
		lp, e := strings.ToLower(pw), strings.ToLower(email)
		local := e
		if at := strings.LastIndex(e, "@"); at > 0 {
			local = e[:at]
		}
		if lp == e || lp == local {
			return "must not be your email address"
		}
	}
	return ""
}

func checkDisplayName(s string) string {
	switch n := utf8.RuneCountInString(s); {
	case n == 0:
		return "is required"
	case n > maxNameRunes:
		return "must be at most 80 characters"
	case strings.IndexFunc(s, unicode.IsControl) >= 0:
		return "must not contain control characters"
	}
	return ""
}

func checkAvatarURL(s string) string {
	if len(s) > maxAvatarURLLen {
		return "is too long"
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" || strings.ContainsAny(s, " \t\r\n") {
		return "must be an https URL"
	}
	return ""
}

func checkEmail(s string) string {
	if s == "" {
		return "is required"
	}
	if len(s) > maxEmailLen {
		return "is too long"
	}
	addr, err := mail.ParseAddress(s)
	if err != nil || addr.Address != s {
		return "must be a valid email address"
	}
	at := strings.LastIndex(s, "@")
	domain := s[at+1:]
	if !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return "must be a valid email address"
	}
	return ""
}
