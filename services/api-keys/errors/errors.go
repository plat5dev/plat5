package errors

import (
	"encoding/json"
	"fmt"

	"github.com/gofiber/fiber/v3"
)

// ErrorKind represents standard error categories for telemetry.
type ErrorKind string

// Closed set: auth, network, db, io, internal, validation.
const (
	KindAuth       ErrorKind = "auth"
	KindNetwork    ErrorKind = "network"
	KindDB         ErrorKind = "db"
	KindIO         ErrorKind = "io"
	KindInternal   ErrorKind = "internal"
	KindValidation ErrorKind = "validation"
)

func (k ErrorKind) String() string {
	return string(k)
}

// ApiError is a Plat5-standardized API error.
// It is wrapped in an envelope when serialized:
//
//	{"error": {"type": "...", "code": "...", "message": "...", "request_id": "...", "details": ...}}
type ApiError struct {
	Type    string
	Code    string
	Message string
	Details interface{}
	Status  int
	Kind    ErrorKind
}

// Error implements the error interface.
func (e *ApiError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Common error constructors

func ValidationError(message string, details interface{}) *ApiError {
	return &ApiError{
		Type:    "invalid_request_error",
		Code:    "VALIDATION_ERROR",
		Message: message,
		Details: details,
		Status:  fiber.StatusUnprocessableEntity,
	}
}

func NotFoundError(resource string, id interface{}) *ApiError {
	return &ApiError{
		Type:    "invalid_request_error",
		Code:    "NOT_FOUND",
		Message: "Resource not found",
		Details: map[string]interface{}{
			"resource": resource,
			"id":       id,
		},
		Status: fiber.StatusNotFound,
	}
}

func ConflictError(field string, value interface{}) *ApiError {
	return &ApiError{
		Type:    "invalid_request_error",
		Code:    "CONFLICT",
		Message: "Resource already exists",
		Details: map[string]interface{}{
			"field": field,
			"value": value,
		},
		Status: fiber.StatusConflict,
	}
}

func PayloadTooLargeError(maxSizeBytes int64) *ApiError {
	return &ApiError{
		Type:    "invalid_request_error",
		Code:    "PAYLOAD_TOO_LARGE",
		Message: "Request body exceeds maximum allowed size",
		Details: map[string]interface{}{
			"max_size_bytes": maxSizeBytes,
		},
		Status: fiber.StatusRequestEntityTooLarge,
	}
}

func RateLimitedError() *ApiError {
	return &ApiError{
		Type:    "rate_limit_error",
		Code:    "RATE_LIMITED",
		Message: "Too many requests",
		Details: nil,
		Status:  fiber.StatusTooManyRequests,
	}
}

func UnauthorizedError(reason string) *ApiError {
	return &ApiError{
		Type:    "invalid_request_error",
		Code:    "UNAUTHORIZED",
		Message: "Authentication required",
		Details: map[string]interface{}{
			"reason": reason,
		},
		Status: fiber.StatusUnauthorized,
		Kind:   KindAuth,
	}
}

func InternalError() *ApiError {
	return &ApiError{
		Type:    "api_error",
		Code:    "INTERNAL_ERROR",
		Message: "An unexpected error occurred",
		Details: nil,
		Status:  fiber.StatusInternalServerError,
		Kind:    KindInternal,
	}
}

func ServiceUnavailableError() *ApiError {
	return &ApiError{
		Type:    "api_error",
		Code:    "SERVICE_UNAVAILABLE",
		Message: "Service temporarily unavailable",
		Details: nil,
		Status:  fiber.StatusServiceUnavailable,
		Kind:    KindNetwork,
	}
}

// errorEnvelope is the JSON shape returned to clients.
type errorEnvelope struct {
	Error struct {
		Type      string      `json:"type"`
		Code      string      `json:"code"`
		Message   string      `json:"message"`
		RequestID *string     `json:"request_id"`
		Details   interface{} `json:"details"`
	} `json:"error"`
}

// NewErrorResponse builds the Plat5-standard JSON envelope.
func (e *ApiError) Response(requestID string) fiber.Map {
	env := errorEnvelope{}
	env.Error.Type = e.Type
	env.Error.Code = e.Code
	env.Error.Message = e.Message
	env.Error.Details = e.Details
	if requestID != "" {
		env.Error.RequestID = &requestID
	}

	// Serialize through JSON to ensure the envelope shape is exact,
	// then deserialize back to a map so Fiber can encode it.
	b, _ := json.Marshal(env)
	var out fiber.Map
	_ = json.Unmarshal(b, &out)
	return out
}

// FiberErrorHandler is the centralized error handler for the Fiber app.
// It converts all errors into the Plat5-standard envelope.
func FiberErrorHandler(c fiber.Ctx, err error) error {
	// Default to internal error
	apiErr := InternalError()

	switch e := err.(type) {
	case *ApiError:
		apiErr = e
	case *fiber.BindError:
		// v3 structured binding errors — map to VALIDATION_ERROR with field details.
		details := map[string]interface{}{
			"fields": []map[string]string{
				{"path": e.Field, "message": e.Err.Error()},
			},
		}
		apiErr = ValidationError("Request validation failed", details)
	case *fiber.Error:
		// Fiber's built-in errors (e.g., from c.Status().SendString())
		// Map common codes to our standard errors.
		switch e.Code {
		case fiber.StatusBadRequest:
			apiErr = ValidationError(e.Message, nil)
		case fiber.StatusUnauthorized:
			// Downstream services must not return UNAUTHORIZED (401).
			// Per gateway-contract.md, missing auth context is a gateway bug => INTERNAL_ERROR (500).
			apiErr = InternalError()
		case fiber.StatusNotFound:
			apiErr = NotFoundError("resource", nil)
		case fiber.StatusConflict:
			apiErr = ConflictError("", nil)
		case fiber.StatusRequestEntityTooLarge:
			apiErr = PayloadTooLargeError(0)
		case fiber.StatusTooManyRequests:
			apiErr = RateLimitedError()
		case fiber.StatusServiceUnavailable:
			apiErr = ServiceUnavailableError()
		default:
			apiErr = InternalError()
			apiErr.Message = e.Message
		}
	}

	// Extract request_id from the gateway-injected header. Services must not generate request IDs.
	requestID := c.Get("X-Request-ID")

	return c.Status(apiErr.Status).JSON(apiErr.Response(requestID))
}
