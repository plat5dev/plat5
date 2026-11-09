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

const FallbackValidation = "That doesn't look right."

func ValidationError(message string, details interface{}) *ApiError {
	if message == "" {
		message = FallbackValidation
	}
	return &ApiError{
		Type:    "invalid_request_error",
		Code:    "VALIDATION_ERROR",
		Message: message,
		Details: details,
		Status:  fiber.StatusUnprocessableEntity,
		Kind:    KindValidation,
	}
}

func InvalidRequestError() *ApiError {
	return &ApiError{
		Type:    "invalid_request_error",
		Code:    "INVALID_REQUEST",
		Message: "Malformed request.",
		Details: nil,
		Status:  fiber.StatusBadRequest,
		Kind:    KindValidation,
	}
}

// InvalidRequestMessage is INVALID_REQUEST with a message saying what's
// wrong, for a body identity can't read where the API calls for a 400.
func InvalidRequestMessage(message string) *ApiError {
	e := InvalidRequestError()
	if message != "" {
		e.Message = message
	}
	return e
}

func FieldError(path, message string) *ApiError {
	if message == "" {
		message = FallbackValidation
	}
	return ValidationFields(message, Field{Path: path, Message: message})
}

func ValidationFields(message string, fields ...Field) *ApiError {
	if message == "" {
		message = FallbackValidation
	}
	return ValidationError(message, map[string]interface{}{"fields": fields})
}

func NotFoundError(resource string, id interface{}) *ApiError {
	return &ApiError{
		Type:    "invalid_request_error",
		Code:    "NOT_FOUND",
		Message: "Resource not found.",
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
		Message: "You don't have permission to do that.",
		Details: map[string]interface{}{
			"permission":  permission,
			"resource":    resource,
			"resource_id": resourceID,
		},
		Status: fiber.StatusForbidden,
		Kind:   KindAuth,
	}
}

func ConflictError(message, field string, value interface{}) *ApiError {
	if message == "" {
		message = "That already exists."
	}
	return &ApiError{
		Type:    "invalid_request_error",
		Code:    "CONFLICT",
		Message: message,
		Details: map[string]interface{}{
			"field": field,
			"value": value,
		},
		Status: fiber.StatusConflict,
		Kind:   KindValidation,
	}
}

// StateError is a 409 whose code names the state that refuses the request
// (docs/reference/errors.md#which-status). CONFLICT is only for something
// that already exists; each other refusal has its own code.
func StateError(code, message string, details interface{}) *ApiError {
	return &ApiError{
		Type:    "invalid_request_error",
		Code:    code,
		Message: message,
		Details: details,
		Status:  fiber.StatusConflict,
		Kind:    KindValidation,
	}
}

// The 409 codes for state refusals, each with its fallback message
// (docs/reference/error-copy.md).
const (
	CodeParentDeleted       = "PARENT_DELETED"
	CodeGraceExpired        = "GRACE_EXPIRED"
	CodeNodePurged          = "NODE_PURGED"
	CodeSlugNotReleasable   = "SLUG_NOT_RELEASABLE"
	CodeInviteNotRedeemable = "INVITE_NOT_REDEEMABLE"
	CodeNameOwnedByIdP      = "NAME_OWNED_BY_IDP"
	CodeCursorAhead         = "CURSOR_AHEAD"
)

// ParentDeletedError: a restore or a create under a node that isn't live.
func ParentDeletedError(parentKind, parentID string) *ApiError {
	return StateError(CodeParentDeleted, fmt.Sprintf("Restore the %s first.", parentKind),
		map[string]interface{}{"parent_id": parentID})
}

// GraceExpiredError: a restore past the grace period.
func GraceExpiredError() *ApiError {
	return StateError(CodeGraceExpired, "This can no longer be restored.", nil)
}

// NodePurgedError: a write to a purged node, or to what hangs off it.
func NodePurgedError(kind string) *ApiError {
	return StateError(CodeNodePurged, fmt.Sprintf("This %s was permanently deleted.", kind), nil)
}

// SlugNotReleasableError: a slug release on a node that isn't a purged root.
func SlugNotReleasableError() *ApiError {
	return StateError(CodeSlugNotReleasable, "Only a purged root's slug can be released.", nil)
}

// InviteNotRedeemableError: a redeem of an invite that's redeemed, revoked or
// expired, or whose node is deleted (reason node_deleted).
func InviteNotRedeemableError(message, reason string) *ApiError {
	return StateError(CodeInviteNotRedeemable, message, map[string]interface{}{"reason": reason})
}

// NameOwnedByIdPError: a name edit under users.name.sync each_sign_in.
func NameOwnedByIdPError() *ApiError {
	return StateError(CodeNameOwnedByIdP, "This name comes from the sign-in provider.", nil)
}

// CursorAheadError: a lifecycle read whose after is past the log's newest
// event. The log was reset; the reader starts over from the first event.
func CursorAheadError() *ApiError {
	return StateError(CodeCursorAhead, "The log has no event at this cursor. Read from its first event.", nil)
}

func PayloadTooLargeError(maxSizeBytes int64) *ApiError {
	return &ApiError{
		Type:    "invalid_request_error",
		Code:    "PAYLOAD_TOO_LARGE",
		Message: "Request body is too large.",
		Details: map[string]interface{}{
			"max_size_bytes": maxSizeBytes,
		},
		Status: fiber.StatusRequestEntityTooLarge,
		Kind:   KindValidation,
	}
}

func UnauthorizedError(reason string) *ApiError {
	return &ApiError{
		Type:    "invalid_request_error",
		Code:    "UNAUTHORIZED",
		Message: "Authentication required.",
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
		Message: "An unexpected error occurred.",
		Details: nil,
		Status:  fiber.StatusInternalServerError,
		Kind:    KindInternal,
	}
}

func ServiceUnavailableError() *ApiError {
	return &ApiError{
		Type:    "api_error",
		Code:    "SERVICE_UNAVAILABLE",
		Message: "Service temporarily unavailable.",
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

// FromError maps a handler error to the envelope FiberErrorHandler writes.
// The request logger uses it too, so the logged status is the returned status.
func FromError(err error) *ApiError {
	apiErr := InternalError()

	switch e := err.(type) {
	case *ApiError:
		apiErr = e
	case *fiber.BindError:
		// A field of the wrong type is invalid input; a body that can't be
		// read at all names no field and is malformed.
		if e.Field == "" {
			apiErr = InvalidRequestError()
		} else {
			apiErr = FieldError(e.Field, FallbackValidation)
		}
	case *fiber.Error:
		switch e.Code {
		case fiber.StatusBadRequest:
			apiErr = InvalidRequestError()
		case fiber.StatusUnauthorized:
			apiErr = UnauthorizedError("unauthorized")
		case fiber.StatusForbidden:
			apiErr = ForbiddenError("", "resource", nil)
		case fiber.StatusNotFound:
			apiErr = NotFoundError("resource", nil)
		case fiber.StatusConflict:
			apiErr = ConflictError("", "", nil)
		case fiber.StatusRequestEntityTooLarge:
			apiErr = PayloadTooLargeError(0)
		case fiber.StatusServiceUnavailable:
			apiErr = ServiceUnavailableError()
		default:
			// A client error is never a 500: any other 4xx from Fiber, such as
			// its 422 for a body it can't bind, is a malformed request.
			if e.Code >= 400 && e.Code < 500 {
				apiErr = InvalidRequestError()
			}
		}
	}
	return apiErr
}

func FiberErrorHandler(c fiber.Ctx, err error) error {
	apiErr := FromError(err)
	requestID := c.Get("X-Request-ID")
	return c.Status(apiErr.Status).JSON(apiErr.Response(requestID))
}
