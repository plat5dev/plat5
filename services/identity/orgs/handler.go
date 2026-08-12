package orgs

import (
	"context"
	stderrors "errors"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/httpx"
	"github.com/plat5dev/plat5/identity/telemetry"
)

const tracerName = "identity.handlers"

type Handler struct {
	store  *Store
	tracer trace.Tracer
	telem  *telemetry.Telemetry
}

func NewHandler(store *Store, telem *telemetry.Telemetry) *Handler {
	return &Handler{
		store:  store,
		tracer: otel.Tracer(tracerName),
		telem:  telem,
	}
}

type storeErrOpts struct {
	notFoundResource string
	notFoundID       interface{}
	conflictField    string
	conflictValue    interface{}
}

func notFound(resource string, id interface{}) storeErrOpts {
	return storeErrOpts{notFoundResource: resource, notFoundID: id}
}

func conflict(field string, value interface{}) storeErrOpts {
	return storeErrOpts{conflictField: field, conflictValue: value}
}

// mapStoreErr maps store sentinel errors and ApiError through; logs unexpected errors.
func (h *Handler) mapStoreErr(ctx context.Context, span trace.Span, err error, msg string, opts ...storeErrOpts) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(*errors.ApiError); ok {
		return err
	}

	var o storeErrOpts
	for _, opt := range opts {
		if opt.notFoundResource != "" {
			o.notFoundResource = opt.notFoundResource
			o.notFoundID = opt.notFoundID
		}
		if opt.conflictField != "" {
			o.conflictField = opt.conflictField
			o.conflictValue = opt.conflictValue
		}
	}

	if stderrors.Is(err, ErrNotFound) && o.notFoundResource != "" {
		return errors.NotFoundError(o.notFoundResource, o.notFoundID)
	}
	if stderrors.Is(err, ErrConflict) && o.conflictField != "" {
		return errors.ConflictError(o.conflictField, o.conflictValue)
	}

	h.logError(ctx, span, msg, err, errors.KindDB)
	return errors.InternalError()
}

func requireName(raw, path string, maxLen int) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", errors.FieldError(path, "required")
	}
	if len(name) > maxLen {
		return "", errors.FieldError(path, "must be at most 128 characters")
	}
	return name, nil
}

// requireActiveMember returns 404 for non-member / unknown org (existence policy).
func (h *Handler) requireActiveMember(ctx context.Context, orgID, userID string) (*Member, error) {
	m, err := h.store.GetActiveMemberForUser(ctx, orgID, userID)
	if err != nil {
		if stderrors.Is(err, ErrNotFound) {
			return nil, errors.NotFoundError("organization", orgID)
		}
		h.logError(ctx, nil, "failed to load member", err, errors.KindDB)
		return nil, errors.InternalError()
	}
	return m, nil
}

func (h *Handler) logError(ctx context.Context, span trace.Span, msg string, err error, kind errors.ErrorKind) {
	httpx.LogError(ctx, span, h.telem.LoggerWithContext(ctx), msg, err, kind)
}
