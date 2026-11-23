package reference

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
)

// The error codes of README.md T-5 this server answers.
const (
	codeInvalidRequest      = "invalid_request"
	codeRegistrationRefused = "registration_refused"
	codeInvalidProof        = "invalid_proof"
	codeTooManyKeys         = "too_many_keys"
	codeCursorExpired       = "cursor_expired"
	codeReportTooLarge      = "report_too_large"
	codeThrottled           = "throttled"
	codeUnauthorized        = "unauthorized"
	codeNotFound            = "not_found"
)

// internalError is the message of every 500; what failed goes to the log alone.
const internalError = "internal error"

// refusal is an answer other than success: its status, its T-5 code and a message naming no secret.
type refusal struct {
	status     int
	code       string
	message    string
	retryAfter int
}

func (r *refusal) Error() string { return fmt.Sprintf("%d %s: %s", r.status, r.code, r.message) }

func refuse(status int, code, format string, args ...any) *refusal {
	return &refusal{status: status, code: code, message: fmt.Sprintf(format, args...)}
}

// bodyTooLarge answers a body over a route's bound, on a route with no code of its own for it.
var bodyTooLarge = refuse(http.StatusBadRequest, codeInvalidRequest, "the body is over this route's bound")

// handler answers a request, or returns the error the route answers instead.
type handler func(w http.ResponseWriter, r *http.Request) error

// errorBody is T-5's error answer.
type errorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// serve is the one place a route's error becomes an answer: a refusal as T-5 shapes it, anything
// else as 500. A panic is answered 500 too, rather than dropping the connection.
func serve(log *slog.Logger, h handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				log.Error("route panicked", "trace_id", traceID(), "method", r.Method, "path", r.URL.Path, "panic", p)
				writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal", Message: internalError})
			}
		}()
		err := h(w, r)
		if err == nil {
			return
		}
		if refused, ok := errors.AsType[*refusal](err); ok {
			if refused.retryAfter > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(refused.retryAfter))
			}
			writeJSON(w, refused.status, errorBody{Error: refused.code, Message: refused.message})
			return
		}
		log.Error("route failed", "trace_id", traceID(), "method", r.Method, "path", r.URL.Path, "error", err)
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal", Message: internalError})
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// decodeBody reads a JSON body of at most limit bytes into dest, ignoring members it does not know (T-4).
// A longer body is answered with tooLarge.
func decodeBody(w http.ResponseWriter, r *http.Request, limit int64, tooLarge *refusal, dest any) error {
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(dest)
	var over *http.MaxBytesError
	switch {
	case errors.As(err, &over):
		return tooLarge
	case err != nil:
		return refuse(http.StatusBadRequest, codeInvalidRequest, "the body is not the JSON this route takes")
	}
	return nil
}

// traceID is a locally generated trace identifier for a log line; this server runs no tracer.
func traceID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
