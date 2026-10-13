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

// Credential kinds the gateway admits. Members on organization and member
// routes; a person on user routes.
const (
	AuthMemberAPIKey  = "member_apikey"
	AuthMemberSession = "member_session"
	AuthJWT           = "jwt"
	AuthUserAPIKey    = "user_apikey"
)

// Actor is the credential that acted. MemberID is nil on a person's event
// until the service names the member; KeyPrefix is nil for a JWT.
type Actor struct {
	MemberID  *string `json:"member_id"`
	AuthType  string  `json:"auth_type"`
	KeyPrefix *string `json:"key_prefix"`
}

// Intent is what the gateway knows before forward. Exactly one of
// OrganizationID (an org's event) or UserID (a person's) is set.
type Intent struct {
	RequestID      string
	OccurredAt     time.Time
	OrganizationID *string
	UserID         *string
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
//
// CreatedID is the id a create produced. Named is the org a user route
// changed, and the member in it: it adds the event to that org's log. Both
// only on responded.
type Outcome struct {
	Outcome   string
	Status    *int
	Details   json.RawMessage
	CreatedID *string
	Named     *Named
}

// Named is the org and member a service named on a user route.
type Named struct {
	OrganizationID string
	MemberID       string
}

type Event struct {
	ID string
	Intent
	Outcome
}

// Filter is one log's list query: an org's or a person's, by which is set.
// Empty strings and nil times do not filter.
type Filter struct {
	OrganizationID string
	UserID         string
	CreatedID      string
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
