package access

import (
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/plat5dev/plat5/identity/feed"
	"github.com/plat5dev/plat5/identity/metrics"
)

// ErrPDPUnavailable is returned when an external AuthZEN PDP fails, times out,
// or answers with an invalid payload (mapped to 503 SERVICE_UNAVAILABLE).
var ErrPDPUnavailable = stderrors.New("external authorizer unavailable")

// MaxPDPBatch is the maximum number of items sent in one outbound
// POST /access/v1/evaluations call.
const MaxPDPBatch = 100

// AuthzenSubject is an OpenID AuthZEN 1.0 subject.
type AuthzenSubject struct {
	Type       string         `json:"type"`
	ID         string         `json:"id"`
	Properties map[string]any `json:"properties,omitempty"`
}

// AuthzenAction is an OpenID AuthZEN 1.0 action.
type AuthzenAction struct {
	Name       string         `json:"name"`
	Properties map[string]any `json:"properties,omitempty"`
}

// AuthzenResource is an OpenID AuthZEN 1.0 resource.
type AuthzenResource struct {
	Type       string         `json:"type"`
	ID         string         `json:"id"`
	Properties map[string]any `json:"properties,omitempty"`
}

// AuthzenRequest is one item of an OpenID AuthZEN 1.0 evaluation or batch.
type AuthzenRequest struct {
	Subject  AuthzenSubject  `json:"subject"`
	Action   AuthzenAction   `json:"action"`
	Resource AuthzenResource `json:"resource"`
	Context  map[string]any  `json:"context,omitempty"`
}

// PDP evaluates authorization requests against an external AuthZEN 1.0 server.
type PDP interface {
	EvaluateBatch(ctx context.Context, authz feed.Authorizer, reqs []AuthzenRequest) ([]bool, error)
}

// HTTPPDP calls an external OpenID AuthZEN 1.0 PDP over HTTP.
type HTTPPDP struct {
	client *http.Client
}

func NewHTTPPDP() *HTTPPDP {
	return &HTTPPDP{client: &http.Client{}}
}

type authzenBatchBody struct {
	Options     authzenBatchOptions `json:"options"`
	Evaluations []AuthzenRequest    `json:"evaluations"`
}

type authzenBatchOptions struct {
	EvaluationsSemantic string `json:"evaluations_semantic"`
}

type authzenBatchAnswer struct {
	Evaluations []struct {
		Decision bool `json:"decision"`
	} `json:"evaluations"`
}

func (p *HTTPPDP) EvaluateBatch(ctx context.Context, authz feed.Authorizer, reqs []AuthzenRequest) ([]bool, error) {
	if len(reqs) == 0 {
		return nil, nil
	}
	out := make([]bool, 0, len(reqs))
	for start := 0; start < len(reqs); start += MaxPDPBatch {
		end := min(start+MaxPDPBatch, len(reqs))
		chunk, err := p.evalChunk(ctx, authz, reqs[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, chunk...)
	}
	return out, nil
}

func (p *HTTPPDP) evalChunk(ctx context.Context, authz feed.Authorizer, reqs []AuthzenRequest) ([]bool, error) {
	started := time.Now()
	timeout := authz.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var (
		endpoint string
		op       string
		payload  []byte
		err      error
	)
	if len(reqs) == 1 {
		endpoint = authz.URL + "/access/v1/evaluation"
		op = "evaluation"
		payload, err = json.Marshal(reqs[0])
	} else {
		endpoint = authz.URL + "/access/v1/evaluations"
		op = "evaluations"
		payload, err = json.Marshal(authzenBatchBody{
			Options:     authzenBatchOptions{EvaluationsSemantic: ExecuteAll},
			Evaluations: reqs,
		})
	}
	if err != nil {
		return nil, fmt.Errorf("%w: marshal: %v", ErrPDPUnavailable, err)
	}

	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("%w: request: %v", ErrPDPUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if authz.Token != "" {
		req.Header.Set("Authorization", "Bearer "+authz.Token)
	}
	otel.GetTextMapPropagator().Inject(callCtx, propagation.HeaderCarrier(req.Header))

	resp, err := p.client.Do(req)
	if err != nil {
		metrics.RecordAuthorizerRequest(authz.Name, op, "error", time.Since(started))
		return nil, fmt.Errorf("%w: %v", ErrPDPUnavailable, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		metrics.RecordAuthorizerRequest(authz.Name, op, "error", time.Since(started))
		return nil, fmt.Errorf("%w: read: %v", ErrPDPUnavailable, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		metrics.RecordAuthorizerRequest(authz.Name, op, "error", time.Since(started))
		return nil, fmt.Errorf("%w: status %d", ErrPDPUnavailable, resp.StatusCode)
	}

	if len(reqs) == 1 {
		var single struct {
			Decision *bool `json:"decision"`
		}
		if err := json.Unmarshal(body, &single); err != nil || single.Decision == nil {
			metrics.RecordAuthorizerRequest(authz.Name, op, "error", time.Since(started))
			return nil, fmt.Errorf("%w: invalid evaluation response", ErrPDPUnavailable)
		}
		res := "deny"
		if *single.Decision {
			res = "allow"
		}
		metrics.RecordAuthorizerRequest(authz.Name, op, res, time.Since(started))
		return []bool{*single.Decision}, nil
	}

	var batch authzenBatchAnswer
	if err := json.Unmarshal(body, &batch); err != nil || len(batch.Evaluations) != len(reqs) {
		metrics.RecordAuthorizerRequest(authz.Name, op, "error", time.Since(started))
		return nil, fmt.Errorf("%w: invalid evaluations response", ErrPDPUnavailable)
	}
	decisions := make([]bool, len(reqs))
	for i, e := range batch.Evaluations {
		decisions[i] = e.Decision
	}
	metrics.RecordAuthorizerRequest(authz.Name, op, "ok", time.Since(started))
	return decisions, nil
}

// NeedsExternal reports whether a kind authorizer with mode requires calling
// the external PDP given the built-in RBAC decision.
func NeedsExternal(mode string, builtin bool) bool {
	switch mode {
	case feed.AuthorizerModeAnd:
		return builtin
	case feed.AuthorizerModeOr:
		return !builtin
	default: // replace
		return true
	}
}
