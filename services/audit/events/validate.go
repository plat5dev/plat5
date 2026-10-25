package events

import (
	"bytes"
	"encoding/json"
	stderrors "errors"
	"time"

	"github.com/plat5dev/plat5/audit/errors"
)

// The gateway decides an event's fields; audit stores what it's sent
// (docs/reference/api/internal.md#audit). A body is refused only for what
// can't be stored: a missing occurred_at or service, or a value of the wrong
// type. Unknown fields are ignored.

type intentRequest struct {
	OccurredAt *string           `json:"occurred_at"`
	Nodes      []Node            `json:"nodes"`
	Actor      Actor             `json:"actor"`
	Service    *string           `json:"service"`
	Method     *string           `json:"method"`
	Route      *string           `json:"route"`
	Params     map[string]string `json:"params"`
	IP         *string           `json:"ip"`
	UserAgent  *string           `json:"user_agent"`
	// A gateway refusal: the intent arrives final.
	Outcome *string `json:"outcome"`
	Status  *int    `json:"status"`
}

type outcomeRequest struct {
	Outcome       *string         `json:"outcome"`
	Status        *int            `json:"status"`
	Details       json.RawMessage `json:"details"`
	CreatedID     *string         `json:"created_id"`
	NamedNodes    []string        `json:"named_nodes"`
	NamedMemberID *string         `json:"named_member_id"`
}

// decode unmarshals body into dst. A type error names its field.
func decode(body []byte, dst any) error {
	if err := json.Unmarshal(body, dst); err != nil {
		var typeErr *json.UnmarshalTypeError
		if stderrors.As(err, &typeErr) && typeErr.Field != "" {
			return errors.FieldError(typeErr.Field, "")
		}
		return errors.FieldError("body", "")
	}
	return nil
}

// ParseIntent reads an intent body for requestID. A refusal's intent carries
// its outcome, returned as final; otherwise the outcome is nil.
func ParseIntent(requestID string, body []byte) (*Intent, *Outcome, error) {
	if requestID == "" {
		return nil, nil, errors.FieldError("request_id", "")
	}
	var req intentRequest
	if err := decode(body, &req); err != nil {
		return nil, nil, err
	}
	if req.OccurredAt == nil {
		return nil, nil, errors.FieldError("occurred_at", "")
	}
	at, err := time.Parse(time.RFC3339Nano, *req.OccurredAt)
	if err != nil {
		return nil, nil, errors.FieldError("occurred_at", "")
	}
	if req.Service == nil || *req.Service == "" {
		return nil, nil, errors.FieldError("service", "")
	}

	in := &Intent{
		RequestID: requestID,
		// The event is to the millisecond, and so is its id.
		OccurredAt: at.UTC().Truncate(time.Millisecond),
		Nodes:      req.Nodes,
		Actor:      req.Actor,
		Service:    *req.Service,
		Method:     req.Method,
		Route:      req.Route,
		Params:     req.Params,
		IP:         req.IP,
		UserAgent:  req.UserAgent,
	}
	if in.Nodes == nil {
		in.Nodes = []Node{}
	}
	if in.Params == nil {
		in.Params = map[string]string{}
	}
	if len(in.Nodes) > 0 {
		root := in.Nodes[0].ID
		in.RootID = &root
	}

	if req.Outcome == nil || *req.Outcome == OutcomePending {
		return in, nil, nil
	}
	return in, &Outcome{Outcome: *req.Outcome, Status: req.Status, NamedNodes: []string{}}, nil
}

// ParseOutcome reads an outcome body. Pending is not an outcome.
func ParseOutcome(requestID string, body []byte) (*Outcome, error) {
	if requestID == "" {
		return nil, errors.FieldError("request_id", "")
	}
	var req outcomeRequest
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	if req.Outcome == nil || *req.Outcome == "" || *req.Outcome == OutcomePending {
		return nil, errors.FieldError("outcome", "")
	}
	out := &Outcome{
		Outcome:       *req.Outcome,
		Status:        req.Status,
		CreatedID:     req.CreatedID,
		NamedNodes:    req.NamedNodes,
		NamedMemberID: req.NamedMemberID,
	}
	if out.NamedNodes == nil {
		out.NamedNodes = []string{}
	}
	details := bytes.TrimSpace(req.Details)
	if len(details) > 0 && !bytes.Equal(details, []byte("null")) {
		if details[0] != '{' {
			return nil, errors.FieldError("details", "")
		}
		out.Details = json.RawMessage(details)
	}
	return out, nil
}
