package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Stable machine-readable error codes. Clients branch on these, not on status
// codes alone and never on message text, so they are part of the API contract.
const (
	CodeUnauthorized         = "unauthorized"
	CodeValidation           = "validation_error"
	CodeNotFound             = "not_found"
	CodeSlugConflict         = "slug_conflict"
	CodeScheduleConflict     = "schedule_conflict"
	CodeVersionConflict      = "version_conflict"
	CodePreconditionRequired = "precondition_required"
	CodeInternal             = "internal_error"
)

// errorBody is the single error envelope every failing endpoint returns.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// writeError renders the error envelope. Handlers use this rather than
// http.Error so that every failure has the same shape.
func writeError(w http.ResponseWriter, status int, code, message string, details map[string]any) {
	writeJSON(w, status, errorBody{Error: errorDetail{
		Code:    code,
		Message: message,
		Details: details,
	}})
}

// writeInternal logs the underlying cause and returns a generic message, so
// internal details never reach the client.
func writeInternal(w http.ResponseWriter, r *http.Request, err error) {
	slog.ErrorContext(r.Context(), "request failed",
		"method", r.Method, "path", r.URL.Path, "error", err)
	writeError(w, http.StatusInternalServerError, CodeInternal,
		"An internal error occurred.", nil)
}

// writeJSON encodes v as the response body.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	// The header and status are already sent, so a failure here cannot be
	// turned into an error response. Log it and move on.
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("encode response body", "error", err)
	}
}
