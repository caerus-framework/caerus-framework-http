package cf_http

import "net/http"

// Failure represents an error that occurred during request processing.
// It contains safe, user-facing information suitable for sending to clients.
// Internal error details should be logged separately, not included in Failure.
type Failure struct {
	// Status is the HTTP status code to send to the client.
	Status int

	// Code is a stable, machine-readable error code (e.g., "NOT_FOUND").
	// It may be empty for plain transport failures.
	Code string

	// Message is a safe, human-readable error message. It must never contain
	// internal causes, stack traces, or user data.
	Message string

	// RequestID is the request ID for correlation with logs, populated from
	// RequestIDFrom(r) by middleware when present.
	RequestID string
}

// ErrorWriter is a function that writes an error response to the client.
// It receives the HTTP response writer, the original request, and the failure.
// Implementations should set appropriate headers, write the status code, and
// never expose internal error details.
type ErrorWriter func(w http.ResponseWriter, r *http.Request, failure Failure)

// DefaultErrorWriter is the default ErrorWriter. It writes Message when
// non-empty, else http.StatusText(Status), via http.Error. Used when a
// middleware's Write option is nil.
func DefaultErrorWriter(w http.ResponseWriter, r *http.Request, failure Failure) {
	message := failure.Message
	if message == "" {
		message = http.StatusText(failure.Status)
	}
	http.Error(w, message, failure.Status)
}

// NewFailure creates a new Failure with the given status, code, and message.
func NewFailure(status int, code, message string) Failure {
	return Failure{
		Status:  status,
		Code:    code,
		Message: message,
	}
}

// WithRequestID sets the request ID on the failure.
func (f Failure) WithRequestID(requestID string) Failure {
	f.RequestID = requestID
	return f
}

// Common error codes
const (
	// ErrorCodeBadRequest indicates invalid request data.
	ErrorCodeBadRequest = "BAD_REQUEST"

	// ErrorCodeUnauthorized indicates missing or invalid authentication.
	ErrorCodeUnauthorized = "UNAUTHORIZED"

	// ErrorCodeForbidden indicates insufficient permissions.
	ErrorCodeForbidden = "FORBIDDEN"

	// ErrorCodeNotFound indicates the requested resource was not found.
	ErrorCodeNotFound = "NOT_FOUND"

	// ErrorCodeConflict indicates a conflict with the current state.
	ErrorCodeConflict = "CONFLICT"

	// ErrorCodeInternal indicates an internal server error.
	ErrorCodeInternal = "INTERNAL_ERROR"

	// ErrorCodeValidation indicates validation errors.
	ErrorCodeValidation = "VALIDATION_ERROR"
)

// BadRequest creates a Failure for HTTP 400 Bad Request.
func BadRequest(code, message string) Failure {
	return NewFailure(http.StatusBadRequest, code, message)
}

// Unauthorized creates a Failure for HTTP 401 Unauthorized.
func Unauthorized(code, message string) Failure {
	return NewFailure(http.StatusUnauthorized, code, message)
}

// Forbidden creates a Failure for HTTP 403 Forbidden.
func Forbidden(code, message string) Failure {
	return NewFailure(http.StatusForbidden, code, message)
}

// NotFound creates a Failure for HTTP 404 Not Found.
func NotFound(code, message string) Failure {
	return NewFailure(http.StatusNotFound, code, message)
}

// Conflict creates a Failure for HTTP 409 Conflict.
func Conflict(code, message string) Failure {
	return NewFailure(http.StatusConflict, code, message)
}

// InternalError creates a Failure for HTTP 500 Internal Server Error.
func InternalError(code, message string) Failure {
	return NewFailure(http.StatusInternalServerError, code, message)
}

// ValidationError creates a Failure for HTTP 422 Unprocessable Entity.
func ValidationError(code, message string) Failure {
	return NewFailure(http.StatusUnprocessableEntity, code, message)
}
