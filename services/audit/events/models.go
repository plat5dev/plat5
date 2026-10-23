package events

import (
	"encoding/json"
	"time"
)

// Outcomes (docs/reference/api/audit.md#event). Pending is the only one an
// intent writes.
const (
	OutcomePending    = "pending"
	OutcomeRejected   = "rejected"
	OutcomeResponded  = "responded"
	OutcomeNoResponse = "no_response"
)

// Who acted, and with which credential.
const (
	PrincipalPerson         = "person"
	PrincipalServiceAccount = "service_account"

	AuthJWT    = "jwt"
	AuthAPIKey = "apikey"
)

// Actor is the member whose credential was admitted. KeyPrefix is nil for a
// JWT.
type Actor struct {
	MemberID  string  `json:"member_id"`
	Principal string  `json:"principal"`
	AuthType  string  `json:"auth_type"`
	KeyPrefix *string `json:"key_prefix"`
}

// Node is one node of the chain a route addressed.
type Node struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Intent is what the gateway knows before forward. On a kind route RootID is
// Nodes[0].ID; on a sign-in route it's nil, Nodes is empty, and the actor is
// the person's directory member.
type Intent struct {
	RequestID  string
	OccurredAt time.Time
	RootID     *string
	Nodes      []Node
	Actor      Actor
	Service    string
	Method     string
	Route      string
	Params     map[string]string
	IP         string
	UserAgent  *string
}

// Outcome is how the request ended. Status is nil when the client was gone
// before an answer, never on responded. Details is nil for null, and only set
// on responded.
//
// CreatedID is the id a create produced. Named is the tree a sign-in route
// changed, and the member in it: it adds the event to that root's log. Both
// only on responded.
type Outcome struct {
	Outcome   string
	Status    *int
	Details   json.RawMessage
	CreatedID *string
	Named     *Named
}

// Named is the root and member a service named on a sign-in route.
type Named struct {
	RootID   string
	MemberID string
}

type Event struct {
	ID string
	Intent
	Outcome
}

// Filter is one log's list query: a node's (the node and below, and the
// trees sign-in routes named) or a person's, by which is set. Empty strings
// and nil times do not filter.
type Filter struct {
	NodeID         string
	MemberID       string
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
