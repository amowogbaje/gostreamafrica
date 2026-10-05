// Package httpx holds the shared HTTP response and middleware helpers.
package httpx

import (
	"encoding/json"
	"net/http"
	"strings"
)

// ErrorBody is the single error format used by every endpoint.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail describes one error. Code is stable and machine-readable.
type ErrorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

// WriteJSON writes v as JSON with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}

// WriteError writes the standard error body, including the request id.
func WriteError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	WriteJSON(w, status, ErrorBody{Error: ErrorDetail{
		Code: code, Message: message, RequestID: RequestIDFrom(r.Context()),
	}})
}

// AllowMethods rejects other methods with a 405 in the standard error format.
func AllowMethods(next http.Handler, methods ...string) http.Handler {
	allow := strings.Join(methods, ", ")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, m := range methods {
			if r.Method == m {
				next.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Allow", allow)
		WriteError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	})
}
