package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// apiError is the single JSON error shape returned by every endpoint.
// Callers can branch on Code reliably; Message is human-readable and
// never contains raw vendor or internal error text.
type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type errorEnvelope struct {
	Error apiError `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{Error: apiError{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Common error codes. Keep this list in sync with what handlers emit —
// it's the public contract callers build retry/alerting logic against.
const (
	codeInvalidCurrency      = "INVALID_CURRENCY"
	codeInvalidAmount        = "INVALID_AMOUNT"
	codeInvalidRequest       = "INVALID_REQUEST"
	codeUpstreamUnavailable  = "UPSTREAM_UNAVAILABLE"
	codeInternal             = "INTERNAL_ERROR"
	codeNotFound             = "NOT_FOUND"
	codeCircuitOpen          = "CIRCUIT_OPEN"
	codeRateLimited          = "RATE_LIMITED"
	codeStaleDataUnavailable = "STALE_DATA_UNAVAILABLE"
)

// writeTooManyRequests writes the standard rate-limit-exceeded response:
// 429 with a Retry-After header and our usual JSON error envelope, so a
// well-behaved caller (or an API gateway in front of us) knows exactly
// how long to back off before trying again instead of guessing.
func writeTooManyRequests(w http.ResponseWriter, retryAfter time.Duration) {
	secs := int(retryAfter.Seconds())
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeError(w, http.StatusTooManyRequests, codeRateLimited, "too many requests, please slow down")
}
