package events

import (
	"bytes"
	"encoding/json"
	stderrors "errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/plat5dev/plat5/audit/errors"
)

// Limits on what the gateway sends (docs/audit.md#event). Over a limit is 422:
// the gateway truncates or caps before it sends, so an overrun is a bug.
const (
	maxIDLen         = 128
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
	OccurredAt     *string           `json:"occurred_at"`
	OrganizationID *string           `json:"organization_id"`
	UserID         *string           `json:"user_id"`
	Actor          *actorRequest     `json:"actor"`
	Service        *string           `json:"service"`
	Method         *string           `json:"method"`
	Route          *string           `json:"route"`
	Params         map[string]string `json:"params"`
	IP             *string           `json:"ip"`
	UserAgent      *string           `json:"user_agent"`
}

type actorRequest struct {
	MemberID  *string `json:"member_id"`
	AuthType  *string `json:"auth_type"`
	KeyPrefix *string `json:"key_prefix"`
}

type outcomeRequest struct {
	Outcome        *string         `json:"outcome"`
	Status         *int            `json:"status"`
	Details        json.RawMessage `json:"details"`
	CreatedID      *string         `json:"created_id"`
	OrganizationID *string         `json:"organization_id"`
	MemberID       *string         `json:"member_id"`
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

	// An org's event: the org and member come from the credential. A person's
	// event: the person; the member comes later, if the service names one.
	orgEvent := req.OrganizationID != nil
	ownerKnown := orgEvent != (req.UserID != nil)
	switch {
	case !ownerKnown:
		bad.add("organization_id")
	case orgEvent:
		in.OrganizationID = optText(&bad, "organization_id", req.OrganizationID, maxIDLen)
	default:
		in.UserID = optText(&bad, "user_id", req.UserID, maxIDLen)
	}
	if req.Actor == nil {
		bad.add("actor")
	} else {
		in.Actor.AuthType = text(&bad, "actor.auth_type", req.Actor.AuthType, maxIDLen)
		member := in.Actor.AuthType == AuthMemberAPIKey || in.Actor.AuthType == AuthMemberSession
		person := in.Actor.AuthType == AuthUserAPIKey || in.Actor.AuthType == AuthJWT
		switch {
		case in.Actor.AuthType == "":
		case !member && !person, ownerKnown && member != orgEvent:
			bad.add("actor.auth_type")
		case member:
			in.Actor.MemberID = optText(&bad, "actor.member_id", req.Actor.MemberID, maxIDLen)
			in.Actor.KeyPrefix = optText(&bad, "actor.key_prefix", req.Actor.KeyPrefix, maxKeyPrefixLen)
		default:
			// A person's credential: no member until the service names one,
			// and no key prefix for a JWT.
			if req.Actor.MemberID != nil {
				bad.add("actor.member_id")
			}
			if in.Actor.AuthType == AuthUserAPIKey {
				in.Actor.KeyPrefix = optText(&bad, "actor.key_prefix", req.Actor.KeyPrefix, maxKeyPrefixLen)
			} else if req.Actor.KeyPrefix != nil {
				bad.add("actor.key_prefix")
			}
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
	if (req.OrganizationID == nil) != (req.MemberID == nil) {
		bad.add("member_id")
	} else if req.OrganizationID != nil {
		out.Named = &Named{
			OrganizationID: text(&bad, "organization_id", req.OrganizationID, maxIDLen),
			MemberID:       text(&bad, "member_id", req.MemberID, maxIDLen),
		}
	}
	if out.Outcome != OutcomeResponded && (req.CreatedID != nil || req.OrganizationID != nil) {
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
