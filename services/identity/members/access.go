package members

import (
	"encoding/json"
	"fmt"

	"github.com/gofiber/fiber/v3"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/httpx"
	"github.com/plat5dev/plat5/identity/internal/id"
	"github.com/plat5dev/plat5/identity/metrics"
)

// MaxEvaluations is the most items one POST /access/v1/evaluations takes.
const MaxEvaluations = 100

// Evaluation semantics (AuthZEN 1.0, section 7.1.2.1).
const (
	ExecuteAll          = "execute_all"
	DenyOnFirstDeny     = "deny_on_first_deny"
	PermitOnFirstPermit = "permit_on_first_permit"
)

// MountAccess serves AuthZEN's access evaluation API on the internal port, at
// /access/v1 (docs/reference/api/internal.md#access-evaluation).
func (h *Handler) MountAccess(r fiber.Router) {
	r.Post("/evaluation", h.Evaluation)
	r.Post("/evaluations", h.Evaluations)
}

type decisionResponse struct {
	Decision bool `json:"decision"`
}

type evaluationsResponse struct {
	Evaluations []decisionResponse `json:"evaluations"`
}

// Evaluation is POST /access/v1/evaluation: one decision.
func (h *Handler) Evaluation(c fiber.Ctx) error {
	echoRequestID(c)
	body, err := readObject(c.Body())
	if err != nil {
		return err
	}
	e, err := readEvaluation(body, "")
	if err != nil {
		return err
	}
	return h.one(c, e)
}

// Evaluations is POST /access/v1/evaluations: a batch, with the top-level
// subject, action and resource as defaults. Without items it's one decision.
func (h *Handler) Evaluations(c fiber.Ctx) error {
	echoRequestID(c)
	body, err := readObject(c.Body())
	if err != nil {
		return err
	}
	defaults, err := readEvaluation(body, "")
	if err != nil {
		return err
	}
	semantic, err := readSemantic(body["options"])
	if err != nil {
		return err
	}
	items, err := readItems(body["evaluations"])
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return h.one(c, defaults)
	}
	evals := make([]Evaluation, len(items))
	for i, raw := range items {
		path := fmt.Sprintf("evaluations[%d]", i)
		obj, err := objectAt(raw, path)
		if err != nil {
			return err
		}
		e, err := readEvaluation(obj, path+".")
		if err != nil {
			return err
		}
		if evals[i], err = e.or(defaults).complete(path); err != nil {
			return err
		}
	}
	decisions, err := h.evaluate(c, evals, semantic)
	if err != nil {
		return err
	}
	out := evaluationsResponse{Evaluations: make([]decisionResponse, len(decisions))}
	for i, d := range decisions {
		out.Evaluations[i] = decisionResponse{Decision: d}
	}
	return c.JSON(out)
}

// one answers e alone, as one decision.
func (h *Handler) one(c fiber.Ctx, e evaluationIn) error {
	ev, err := e.complete("")
	if err != nil {
		return err
	}
	decisions, err := h.evaluate(c, []Evaluation{ev}, ExecuteAll)
	if err != nil {
		return err
	}
	return c.JSON(decisionResponse{Decision: decisions[0]})
}

// evaluate decides evals and returns, and counts, the decisions answered
// under semantic. A batch that stops early is decided whole, in one read, and
// answers only up to where it stops.
func (h *Handler) evaluate(c fiber.Ctx, evals []Evaluation, semantic string) ([]bool, error) {
	decisions, err := h.store.Evaluate(c.Context(), h.src.Current(), evals)
	if err != nil {
		return nil, httpx.MapDB(c.Context(), err, "failed to evaluate access", httpx.DBErr{})
	}
	decisions = answered(decisions, semantic)
	metrics.RecordEvaluations(decisions)
	return decisions, nil
}

// answered is the decisions a batch answers under semantic.
func answered(decisions []bool, semantic string) []bool {
	for i, d := range decisions {
		if (semantic == DenyOnFirstDeny && !d) || (semantic == PermitOnFirstPermit && d) {
			return decisions[:i+1]
		}
	}
	return decisions
}

// echoRequestID returns the caller's X-Request-ID, as AuthZEN asks
// (section 10.1.3), errors included.
func echoRequestID(c fiber.Ctx) {
	if rid := c.Get("X-Request-ID"); rid != "" {
		c.Set("X-Request-ID", rid)
	}
}

// evaluationIn is one evaluation as read: each part nil when it's absent.
// context and properties are read past: accepted and ignored.
type evaluationIn struct {
	subject  *string // the member id
	resource *resourceIn
	action   *string // the permission
}

type resourceIn struct {
	kind, nodeID string
}

// or fills e's missing parts from defaults. A part present replaces the
// default whole.
func (e evaluationIn) or(defaults evaluationIn) evaluationIn {
	if e.subject == nil {
		e.subject = defaults.subject
	}
	if e.resource == nil {
		e.resource = defaults.resource
	}
	if e.action == nil {
		e.action = defaults.action
	}
	return e
}

// complete is e as an Evaluation, or a 400 naming the part it lacks. path is
// where the item sits, "" for the top level.
func (e evaluationIn) complete(path string) (Evaluation, error) {
	for _, missing := range []struct {
		name string
		gone bool
	}{{"subject", e.subject == nil}, {"resource", e.resource == nil}, {"action", e.action == nil}} {
		if !missing.gone {
			continue
		}
		if path == "" {
			return Evaluation{}, badRequest("%s is required.", missing.name)
		}
		return Evaluation{}, badRequest("%s needs a %s, in the item or at the top level.", path, missing.name)
	}
	return Evaluation{MemberID: *e.subject, Kind: e.resource.kind, NodeID: e.resource.nodeID, Permission: *e.action}, nil
}

// readEvaluation reads subject, resource and action from an object, each
// where present. prefix is the object's path with a trailing dot, "" at the
// top level.
func readEvaluation(obj map[string]json.RawMessage, prefix string) (evaluationIn, error) {
	var e evaluationIn
	if s, err := objectAt(obj["subject"], prefix+"subject"); err != nil {
		return e, err
	} else if s != nil {
		typ, err := stringAt(s, prefix+"subject.type")
		if err != nil {
			return e, err
		}
		if typ != "member" {
			return e, badRequest("%ssubject.type must be \"member\".", prefix)
		}
		memberID, err := idAt(s, prefix+"subject.id", "a member id")
		if err != nil {
			return e, err
		}
		e.subject = &memberID
	}
	if r, err := objectAt(obj["resource"], prefix+"resource"); err != nil {
		return e, err
	} else if r != nil {
		kind, err := stringAt(r, prefix+"resource.type")
		if err != nil {
			return e, err
		}
		if kind == "" {
			return e, badRequest("%sresource.type must be a kind name.", prefix)
		}
		nodeID, err := idAt(r, prefix+"resource.id", "a node id")
		if err != nil {
			return e, err
		}
		e.resource = &resourceIn{kind: kind, nodeID: nodeID}
	}
	if a, err := objectAt(obj["action"], prefix+"action"); err != nil {
		return e, err
	} else if a != nil {
		name, err := stringAt(a, prefix+"action.name")
		if err != nil {
			return e, err
		}
		if name == "" {
			return e, badRequest("%saction.name must be a permission.", prefix)
		}
		e.action = &name
	}
	return e, nil
}

// readSemantic is options.evaluations_semantic, execute_all when absent.
// Other options are ignored.
func readSemantic(raw json.RawMessage) (string, error) {
	opts, err := objectAt(raw, "options")
	if err != nil || opts == nil {
		return ExecuteAll, err
	}
	semantic, err := stringAt(opts, "options.evaluations_semantic")
	if err != nil {
		return "", err
	}
	switch semantic {
	case "":
		return ExecuteAll, nil
	case ExecuteAll, DenyOnFirstDeny, PermitOnFirstPermit:
		return semantic, nil
	}
	return "", badRequest("options.evaluations_semantic must be %s, %s or %s.", ExecuteAll, DenyOnFirstDeny, PermitOnFirstPermit)
}

// readItems is the evaluations array, at most MaxEvaluations items.
func readItems(raw json.RawMessage) ([]json.RawMessage, error) {
	if absent(raw) {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, badRequest("evaluations must be an array.")
	}
	if len(items) > MaxEvaluations {
		return nil, badRequest("evaluations holds at most %d items.", MaxEvaluations)
	}
	return items, nil
}

// readObject reads a request body that must be a JSON object.
func readObject(body []byte) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return nil, badRequest("The body must be a JSON object.")
	}
	return obj, nil
}

// objectAt reads raw as an object: nil when it's absent or null.
func objectAt(raw json.RawMessage, path string) (map[string]json.RawMessage, error) {
	if absent(raw) {
		return nil, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, badRequest("%s must be an object.", path)
	}
	return obj, nil
}

// stringAt reads the last key of path from obj as a string: "" when absent
// or null.
func stringAt(obj map[string]json.RawMessage, path string) (string, error) {
	raw := obj[lastKey(path)]
	if absent(raw) {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", badRequest("%s must be a string.", path)
	}
	return s, nil
}

// idAt reads an id: a lowercase ULID, what the API calls what.
func idAt(obj map[string]json.RawMessage, path, what string) (string, error) {
	v, err := stringAt(obj, path)
	if err != nil {
		return "", err
	}
	if !id.Valid(v) {
		return "", badRequest("%s must be %s.", path, what)
	}
	return v, nil
}

func lastKey(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '.' {
			return path[i+1:]
		}
	}
	return path
}

// absent is a key not sent, or sent as null.
func absent(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}

func badRequest(format string, args ...any) error {
	return errors.InvalidRequestMessage(fmt.Sprintf(format, args...))
}
