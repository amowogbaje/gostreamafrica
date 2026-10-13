package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
)

// DecodeJSON reads one JSON object from the request body into dst. It requires
// Content-Type application/json (which also blocks cross-site form posts),
// caps the body at maxBytes, rejects unknown fields and trailing data. On any
// problem it writes the error response itself and returns false.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		WriteError(w, r, http.StatusUnsupportedMediaType, "unsupported_media_type",
			"Content-Type must be application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeDecodeError(w, r, err)
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("trailing data")
		}
		writeDecodeError(w, r, err)
		return false
	}
	return true
}

func writeDecodeError(w http.ResponseWriter, r *http.Request, err error) {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		WriteError(w, r, http.StatusRequestEntityTooLarge, "request_too_large", "request body is too large")
		return
	}
	WriteError(w, r, http.StatusBadRequest, "invalid_json", "request body is not valid for this endpoint")
}
