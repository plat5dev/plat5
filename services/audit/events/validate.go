package events

import (
	"bytes"
	"encoding/json"
	stderrors "errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/plat5dev/plat5/audit/errors"
)

// Limits on what the gateway sends (docs/reference/api/audit.md#event). Over a limit is 422:
// the gateway truncates or caps before it sends, so an overrun is a bug.
const (
	maxIDLen         = 128
	maxKindLen       = 32
	maxKeyPrefixLen  = 64
	maxServiceLen    = 128
	maxRouteLen      = 1024
	maxParams        = 32
	maxParamNameLen  = 64
	maxParamValueLen = 1024
	maxIPLen         = 64
	MaxUserAgentLen  = 512 // characters
	MaxDetailsBytes  = 4096
	// An intent is written as the request arrives. Far from now is a bug, and
	// would ask for a partition nobody maintains.
	maxClockSkew = 24 * time.Hour
)

var auditedMethods = map[string]bool{
	"GET": true, "HEAD": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true,
}

type intentRequest struct {
	OccurredAt *string           `json:"occurred_at"`
	RootID     *string           `json:"root_id"`
	Nodes      *[]nodeRequest    `json:"nodes"`
	Actor      *actorRequest     `json:"actor"`
	Service    *string           `json:"service"`
	Method     *string           `json:"method"`
	Route      *string           `json:"route"`
	Params     map[string]string `json:"params"`
	IP         *string           `json:"ip"`
	UserAgent  *string           `json:"user_agent"`
}

type nodeRequest struct {
	Kind *string `json:"kind"`
	ID   *string `json:"id"`
}

type actorRequest struct {
	MemberID  *string `json:"member_id"`
	Principal *string `json:"principal"`
	AuthType  *string `json:"auth_type"`
	KeyPrefix *string `json:"key_prefix"`
}

type outcomeRequest struct {
	Outcome   *string         `json:"outcome"`
	Status    *int            `json:"status"`
	Details   json.RawMessage `json:"details"`
	CreatedID *string         `json:"created_id"`
	RootID    *string         `json:"root_id"`
	MemberID  *string         `json:"member_id"`
}

type fieldErrs []errors.Field

func (f *fieldErrs) add(path string) {
	*f = append(*f, errors.Field{Path: path, Message: errors.FallbackValidation})
}

func (f fieldErrs) err() error {
	if len(f) == 0 {
		return nil
	}
	return errors.ValidationFields("", f...)
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

// ValidRequestID is the X-Request-ID form: 1-128 of [A-Za-z0-9._-].
func ValidRequestID(s string) bool {
	if len(s) == 0 || len(s) > maxIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-'
		if !ok {
			return false
		}
	}
	return true
}

// ParseIntent validates an intent body for requestID against now.
func ParseIntent(requestID string, body []byte, now time.Time) (*Intent, error) {
	if !ValidRequestID(requestID) {
		return nil, errors.FieldError("request_id", "")
	}
	var req intentRequest
	if err := decode(body, &req); err != nil {
		return nil, err
	}

	var bad fieldErrs
	in := &Intent{RequestID: requestID, Params: map[string]string{}}

	switch t, ok := parseTime(req.OccurredAt); {
	case !ok:
		bad.add("occurred_at")
	case t.Before(now.Add(-maxClockSkew)) || t.After(now.Add(maxClockSkew)):
		bad.add("occurred_at")
	default:
		// The event is to the millisecond, and so is its id.
		in.OccurredAt = t.UTC().Truncate(time.Millisecond)
	}

	// A kind route's event: the chain, root first, and the root names the
	// log. A sign-in route's: no root and no nodes; the person's log is keyed
	// by the actor, their directory member.
	if req.Nodes == nil {
		bad.add("nodes")
	} else {
		in.Nodes = make([]Node, 0, len(*req.Nodes))
		for i, n := range *req.Nodes {
			path := "nodes[" + strconv.Itoa(i) + "]"
			kind := text(&bad, path+".kind", n.Kind, maxKindLen)
			if kind != "" && !validKindName(kind) {
				bad.add(path + ".kind")
			}
			in.Nodes = append(in.Nodes, Node{Kind: kind, ID: text(&bad, path+".id", n.ID, maxIDLen)})
		}
	}
	if req.RootID != nil {
		in.RootID = optText(&bad, "root_id", req.RootID, maxIDLen)
	}
	signIn := req.RootID == nil
	if req.Nodes != nil && (signIn != (len(in.Nodes) == 0) || !signIn && in.RootID != nil && in.Nodes[0].ID != *in.RootID) {
		bad.add("root_id")
	}

	if req.Actor == nil {
		bad.add("actor")
	} else {
		in.Actor.MemberID = text(&bad, "actor.member_id", req.Actor.MemberID, maxIDLen)
		in.Actor.Principal = text(&bad, "actor.principal", req.Actor.Principal, maxIDLen)
		switch in.Actor.Principal {
		case "":
		case PrincipalPerson:
		case PrincipalServiceAccount:
			// A service account has no directory member, so no sign-in route.
			if signIn && req.Nodes != nil && len(in.Nodes) == 0 {
				bad.add("actor.principal")
			}
		default:
			bad.add("actor.principal")
		}
		in.Actor.AuthType = text(&bad, "actor.auth_type", req.Actor.AuthType, maxIDLen)
		switch in.Actor.AuthType {
		case "":
		case AuthAPIKey:
			in.Actor.KeyPrefix = optText(&bad, "actor.key_prefix", req.Actor.KeyPrefix, maxKeyPrefixLen)
		case AuthJWT:
			// Only a person signs in with the IdP, and a JWT has no key prefix.
			if in.Actor.Principal == PrincipalServiceAccount {
				bad.add("actor.auth_type")
			}
			if req.Actor.KeyPrefix != nil {
				bad.add("actor.key_prefix")
			}
		default:
			bad.add("actor.auth_type")
		}
	}
	in.Service = text(&bad, "service", req.Service, maxServiceLen)
	in.Method = text(&bad, "method", req.Method, maxIDLen)
	if in.Method != "" && !auditedMethods[in.Method] {
		bad.add("method")
	}
	in.Route = text(&bad, "route", req.Route, maxRouteLen)
	if in.Route != "" && in.Route[0] != '/' {
		bad.add("route")
	}

	if len(req.Params) > maxParams {
		bad.add("params")
	}
	for name, value := range req.Params {
		if !validParamName(name) || value == "" || len(value) > maxParamValueLen || !utf8.ValidString(value) || strings.Contains(value, "/") {
			bad.add("params." + name)
			continue
		}
		in.Params[name] = value
	}

	in.IP = text(&bad, "ip", req.IP, maxIPLen)
	if req.UserAgent != nil {
		if !utf8.ValidString(*req.UserAgent) || utf8.RuneCountInString(*req.UserAgent) > MaxUserAgentLen {
			bad.add("user_agent")
		} else {
			ua := *req.UserAgent
			in.UserAgent = &ua
		}
	}

	if err := bad.err(); err != nil {
		return nil, err
	}
	return in, nil
}

// ParseOutcome validates an outcome body. Pending is not an outcome.
func ParseOutcome(requestID string, body []byte) (*Outcome, error) {
	if !ValidRequestID(requestID) {
		return nil, errors.FieldError("request_id", "")
	}
	var req outcomeRequest
	if err := decode(body, &req); err != nil {
		return nil, err
	}

	var bad fieldErrs
	out := &Outcome{}
	switch {
	case req.Outcome == nil:
		bad.add("outcome")
	case *req.Outcome == OutcomeRejected, *req.Outcome == OutcomeResponded, *req.Outcome == OutcomeNoResponse:
		out.Outcome = *req.Outcome
	default:
		bad.add("outcome")
	}

	switch {
	case req.Status == nil:
		// The client was gone before an answer. A service that answered has a status.
		if out.Outcome == OutcomeResponded {
			bad.add("status")
		}
	case *req.Status < 100 || *req.Status > 599:
		bad.add("status")
	default:
		s := *req.Status
		out.Status = &s
	}

	details := bytes.TrimSpace(req.Details)
	if len(details) > 0 && !bytes.Equal(details, []byte("null")) {
		// Details come from the upstream response, so only a response has them.
		if out.Outcome != OutcomeResponded || len(details) > MaxDetailsBytes || details[0] != '{' || !json.Valid(details) {
			bad.add("details")
		} else {
			out.Details = json.RawMessage(details)
		}
	}

	// Ids from service headers. The gateway only sends them on a response.
	if req.CreatedID != nil {
		out.CreatedID = optText(&bad, "created_id", req.CreatedID, maxIDLen)
	}
	if (req.RootID == nil) != (req.MemberID == nil) {
		bad.add("member_id")
	} else if req.RootID != nil {
		out.Named = &Named{
			RootID:   text(&bad, "root_id", req.RootID, maxIDLen),
			MemberID: text(&bad, "member_id", req.MemberID, maxIDLen),
		}
	}
	if out.Outcome != OutcomeResponded && (req.CreatedID != nil || req.RootID != nil) {
		bad.add("outcome")
	}

	if err := bad.err(); err != nil {
		return nil, err
	}
	return out, nil
}

func parseTime(s *string) (time.Time, bool) {
	if s == nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, *s)
	return t, err == nil
}

// text is a required single-line string: 1..max bytes of printable ASCII.
func text(bad *fieldErrs, path string, v *string, max int) string {
	if v == nil || *v == "" || len(*v) > max || !printableASCII(*v) {
		bad.add(path)
		return ""
	}
	return *v
}

// optText is text that the caller already knows is present. It returns a
// pointer for nullable columns.
func optText(bad *fieldErrs, path string, v *string, max int) *string {
	s := text(bad, path, v, max)
	if s == "" {
		return nil
	}
	return &s
}

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

func validParamName(s string) bool {
	if s == "" || len(s) > maxParamNameLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

// validKindName is a kind's name: [a-z][a-z0-9_]*, at most 32 (kinds.md).
func validKindName(s string) bool {
	if len(s) == 0 || len(s) > maxKindLen || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}
