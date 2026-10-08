package events

import (
	"encoding/json"
	"time"
)

// Outcomes (docs/audit.md#event). Pending is the only one an intent writes.
const (
	OutcomePending    = "pending"
	OutcomeRejected   = "rejected"
	OutcomeResponded  = "responded"
	OutcomeNoResponse = "no_response"
)

// Credential kinds the gateway admits on organization and member routes.
const (
	AuthMemberAPIKey  = "member_apikey"
	AuthMemberSession = "member_session"
)

type Actor struct {
	MemberID  string `json:"member_id"`
	AuthType  string `json:"auth_type"`
	KeyPrefix string `json:"key_prefix"`
}

// Intent is what the gateway knows before forward.
type Intent struct {
	RequestID      string
	OccurredAt     time.Time
	OrganizationID string
	Actor          Actor
	Service        string
	Method         string
	Route          string
	Params         map[string]string
	IP             string
	UserAgent      *string
}

// Outcome is how the request ended. Status is nil when the client was gone
// before an answer, never on responded. Details is nil for null, and only set
// on responded.
type Outcome struct {
	Outcome string
	Status  *int
	Details json.RawMessage
}

type Event struct {
	ID string
	Intent
	Outcome
}

// Filter is one org's list query. Empty strings and nil times do not filter.
type Filter struct {
	OrganizationID string
	Limit          int
	StartingAfter  string
	ActorMemberID  string
	Service        string
	Method         string
	Route          string
	Outcome        string
	RequestID      string
	Params         map[string]string
	OccurredAfter  *time.Time
	OccurredBefore *time.Time
}
