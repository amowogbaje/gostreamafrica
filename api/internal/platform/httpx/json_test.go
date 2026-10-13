package httpx_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"streamafrica/api/internal/platform/httpx"
)

type sample struct {
	A string `json:"a"`
}

func decode(contentType, body string) (*httptest.ResponseRecorder, bool, sample) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	var dst sample
	ok := httpx.DecodeJSON(rec, req, &dst, 64)
	return rec, ok, dst
}

func TestDecodeJSON(t *testing.T) {
	cases := []struct {
		name, ct, body string
		wantOK         bool
		wantStatus     int
	}{
		{"valid", "application/json", `{"a":"x"}`, true, 0},
		{"valid with charset", "application/json; charset=utf-8", `{"a":"x"}`, true, 0},
		{"missing content type", "", `{"a":"x"}`, false, http.StatusUnsupportedMediaType},
		{"form content type", "application/x-www-form-urlencoded", `a=x`, false, http.StatusUnsupportedMediaType},
		{"unknown field", "application/json", `{"a":"x","b":1}`, false, http.StatusBadRequest},
		{"malformed", "application/json", `{"a":`, false, http.StatusBadRequest},
		{"trailing garbage", "application/json", `{"a":"x"} {"a":"y"}`, false, http.StatusBadRequest},
		{"too large", "application/json", `{"a":"` + strings.Repeat("z", 200) + `"}`, false, http.StatusRequestEntityTooLarge},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec, ok, dst := decode(c.ct, c.body)
			if ok != c.wantOK {
				t.Fatalf("ok=%v body=%s", ok, rec.Body.String())
			}
			if c.wantOK && dst.A != "x" {
				t.Fatalf("decoded %+v", dst)
			}
			if !c.wantOK && rec.Code != c.wantStatus {
				t.Fatalf("status=%d want %d", rec.Code, c.wantStatus)
			}
		})
	}
}

func TestWriteValidationError(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	httpx.WriteValidationError(rec, req, map[string]string{"email": "bad"})
	if rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), `"fields":{"email":"bad"}`) ||
		!strings.Contains(rec.Body.String(), `"code":"validation_failed"`) {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}
