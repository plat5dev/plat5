package errors

import (
	stderrors "errors"
	"testing"

	"github.com/gofiber/fiber/v3"
)

func TestFromErrorNeverTurnsAClientErrorInto500(t *testing.T) {
	for _, code := range []int{
		fiber.StatusMethodNotAllowed,
		fiber.StatusUnsupportedMediaType,
		fiber.StatusUnprocessableEntity,
		fiber.StatusRequestHeaderFieldsTooLarge,
	} {
		got := FromError(fiber.NewError(code))
		if got.Status != fiber.StatusBadRequest || got.Code != "INVALID_REQUEST" {
			t.Errorf("fiber %d: got %d %s, want 400 INVALID_REQUEST", code, got.Status, got.Code)
		}
	}
	if got := FromError(fiber.ErrUnprocessableEntity); got.Status != fiber.StatusBadRequest {
		t.Errorf("Bind().Body's 422: got %d", got.Status)
	}
}

func TestFromErrorKeepsTheStatusesItKnows(t *testing.T) {
	for code, want := range map[int]string{
		fiber.StatusBadRequest:            "INVALID_REQUEST",
		fiber.StatusNotFound:              "NOT_FOUND",
		fiber.StatusRequestEntityTooLarge: "PAYLOAD_TOO_LARGE",
		fiber.StatusServiceUnavailable:    "SERVICE_UNAVAILABLE",
	} {
		got := FromError(fiber.NewError(code))
		if got.Status != code || got.Code != want {
			t.Errorf("fiber %d: got %d %s, want %d %s", code, got.Status, got.Code, code, want)
		}
	}
	if got := FromError(fiber.NewError(fiber.StatusInternalServerError)); got.Status != 500 {
		t.Errorf("fiber 500: got %d", got.Status)
	}
	if got := FromError(stderrors.New("boom")); got.Status != 500 {
		t.Errorf("plain error: got %d", got.Status)
	}
}

func TestFromErrorBindErrors(t *testing.T) {
	if got := FromError(&fiber.BindError{Source: "body", Err: stderrors.New("unexpected EOF")}); got.Status != fiber.StatusBadRequest || got.Code != "INVALID_REQUEST" {
		t.Errorf("unreadable body: got %d %s", got.Status, got.Code)
	}
	if got := FromError(&fiber.BindError{Source: "body", Field: "display_name", Err: stderrors.New("wrong type")}); got.Status != fiber.StatusUnprocessableEntity {
		t.Errorf("wrong type: got %d %s", got.Status, got.Code)
	}
}
