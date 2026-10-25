package events

import (
	"encoding/json"
	"time"
)

// Outcomes (docs/reference/api/audit.md#event). Pending is the one an intent
// writes; a rejected intent arrives final.
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

// Actor is who the gateway admitted. On a kind route MemberID is set; on a
// user route UserID is. KeyID is nil for a JWT.
type Actor struct {
	MemberID  *string `json:"member_id"`
	UserID    *string `json:"user_id"`
	Principal *string `json:"principal"`
	AuthType  *string `json:"auth_type"`
	KeyID     *string `json:"key_id"`
}

// Node is one node of the chain a route addressed.
type Node struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Intent is what the gateway knows before forward. RootID is Nodes[0].ID, nil
// on a user route, whose Nodes is empty.
type Intent struct {
	RequestID  string
	OccurredAt time.Time
	RootID     *string
	Nodes      []Node
	Actor      Actor
	Service    string
	Method     *string
	Route      *string
	Params     map[string]string
	IP         *string
	UserAgent  *string
}

// Outcome is how the request ended. Status is nil when the client was gone
// before an answer. Details is nil for null.
//
// CreatedID is the id a create produced. NamedNodeIDs and NamedMemberID are the
// chain a user route changed and the member in it: they add the event to
// each named node's log.
type Outcome struct {
	Outcome       string
	Status        *int
	Details       json.RawMessage
	CreatedID     *string
	NamedNodeIDs  []string
	NamedMemberID *string
}

type Event struct {
	ID string
	Intent
	Outcome
}

// Filter is one log's list query. Log is the log's key: a node id, or
// UserLog(user id). NodeRead is a node's log, where a user route's event
// shows its named member as the actor and no user id. Empty strings and nil
// pointers do not filter.
type Filter struct {
	Log            string
	NodeRead       bool
	Limit          int
	StartingAfter  string
	ActorMemberID  string
	Service        string
	Method         string
	Route          string
	Outcome        string
	Status         *int
	RequestID      string
	CreatedID      string
	Params         map[string]string
	OccurredAfter  *time.Time
	OccurredBefore *time.Time
}

// UserLog is the log key of a person's own log.
func UserLog(userID string) string {
	return "user:" + userID
}
