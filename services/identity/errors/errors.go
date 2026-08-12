package errors

import (
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
type ApiError struct {
	Type    string
	Code    string
	Message string
	Details interface{}
	Status  int
	Kind    ErrorKind
}

func (e *ApiError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Field is one validation field violation in details.fields.
type Field struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func ValidationError(message string, details interface{}) *ApiError {
	return &ApiError{
		Type:    "invalid_request_error",
		Code:    "VALIDATION_ERROR",
		Message: message,
		Details: details,
		Status:  fiber.StatusUnprocessableEntity,
		Kind:    KindValidation,
	}
}

// FieldError is VALIDATION_ERROR for a single path.
func FieldError(path, message string) *ApiError {
	return ValidationFields("Request validation failed", Field{Path: path, Message: message})
}

// ValidationFields builds VALIDATION_ERROR with details.fields.
func ValidationFields(message string, fields ...Field) *ApiError {
	if message == "" {
		message = "Request validation failed"
	}
	return ValidationError(message, map[string]interface{}{"fields": fields})
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
		Kind:   KindValidation,
	}
}

func ForbiddenError(permission, resource string, resourceID interface{}) *ApiError {
	return &ApiError{
		Type:    "invalid_request_error",
		Code:    "FORBIDDEN",
		Message: "Insufficient permissions",
		Details: map[string]interface{}{
			"permission":  permission,
			"resource":    resource,
			"resource_id": resourceID,
		},
		Status: fiber.StatusForbidden,
		Kind:   KindAuth,
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
		Kind:   KindValidation,
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
		Kind:   KindValidation,
	}
}

func RateLimitedError() *ApiError {
	return &ApiError{
		Type:    "rate_limit_error",
		Code:    "RATE_LIMITED",
		Message: "Too many requests",
		Details: nil,
		Status:  fiber.StatusTooManyRequests,
		Kind:    KindValidation,
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

type errorBody struct {
	Type      string      `json:"type"`
	Code      string      `json:"code"`
	Message   string      `json:"message"`
	RequestID *string     `json:"request_id"`
	Details   interface{} `json:"details"`
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

func (e *ApiError) Response(requestID string) errorEnvelope {
	env := errorEnvelope{
		Error: errorBody{
			Type:    e.Type,
			Code:    e.Code,
			Message: e.Message,
			Details: e.Details,
		},
	}
	if requestID != "" {
		env.Error.RequestID = &requestID
	}
	return env
}

func FiberErrorHandler(c fiber.Ctx, err error) error {
	apiErr := InternalError()

	switch e := err.(type) {
	case *ApiError:
		apiErr = e
	case *fiber.BindError:
		apiErr = FieldError(e.Field, e.Err.Error())
		apiErr.Message = "Request validation failed"
	case *fiber.Error:
		switch e.Code {
		case fiber.StatusBadRequest:
			apiErr = ValidationError(e.Message, nil)
		case fiber.StatusUnauthorized:
			apiErr = UnauthorizedError("unauthorized")
		case fiber.StatusForbidden:
			apiErr = ForbiddenError("", "resource", nil)
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

	requestID := c.Get("X-Request-ID")
	return c.Status(apiErr.Status).JSON(apiErr.Response(requestID))
}
