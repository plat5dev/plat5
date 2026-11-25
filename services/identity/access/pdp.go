package access

import (
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/feed"
	"github.com/plat5dev/plat5/identity/internal/httpx"
	"github.com/plat5dev/plat5/identity/metrics"
)

// ErrPDPUnavailable is returned when the external AuthZEN PDP fails, times out,
// redirects, or answers with an invalid payload (mapped to 503 SERVICE_UNAVAILABLE).
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
	return NewHTTPPDPWithOptions(false)
}

// NewHTTPPDPWithOptions builds an HTTPPDP with connection pooling, redirects
// disabled (fail closed), and optional private-IP SSRF blocking.
func NewHTTPPDPWithOptions(blockPrivateIPs bool) *HTTPPDP {
	dialer := &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	tr := &http.Transport{
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: time.Second,
		DialContext:           dialer.DialContext,
	}
	if blockPrivateIPs {
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			var allowed []net.IP
			for _, ipAddr := range ips {
				if !isBlockedIP(ipAddr.IP) {
					allowed = append(allowed, ipAddr.IP)
				}
			}
			if len(allowed) == 0 {
				return nil, fmt.Errorf("blocked private IP for host %q", host)
			}
			var lastErr error
			for _, ip := range allowed {
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			return nil, lastErr
		}
	}
	return &HTTPPDP{
		client: &http.Client{
			Transport: tr,
			// Never follow redirects: prevents 302 SSRF bypasses and fails closed as non-2xx.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

var cgnatPrefix = netip.MustParsePrefix("100.64.0.0/10")

func isBlockedIP(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	addr = addr.Unmap()
	return addr.IsLoopback() ||
		addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() ||
		addr.IsMulticast() ||
		addr.IsUnspecified() ||
		cgnatPrefix.Contains(addr)
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
		Decision *bool `json:"decision"`
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

const authorizerMetricLabel = "authorizer"

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
		metrics.RecordAuthorizerRequest(authorizerMetricLabel, op, "error", time.Since(started))
		return nil, fmt.Errorf("%w: %v", ErrPDPUnavailable, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		metrics.RecordAuthorizerRequest(authorizerMetricLabel, op, "error", time.Since(started))
		return nil, fmt.Errorf("%w: read: %v", ErrPDPUnavailable, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		metrics.RecordAuthorizerRequest(authorizerMetricLabel, op, "error", time.Since(started))
		return nil, fmt.Errorf("%w: status %d", ErrPDPUnavailable, resp.StatusCode)
	}

	if len(reqs) == 1 {
		var single struct {
			Decision *bool `json:"decision"`
		}
		if err := json.Unmarshal(body, &single); err != nil || single.Decision == nil {
			metrics.RecordAuthorizerRequest(authorizerMetricLabel, op, "error", time.Since(started))
			return nil, fmt.Errorf("%w: invalid evaluation response", ErrPDPUnavailable)
		}
		res := "deny"
		if *single.Decision {
			res = "allow"
		}
		metrics.RecordAuthorizerRequest(authorizerMetricLabel, op, res, time.Since(started))
		return []bool{*single.Decision}, nil
	}

	var batch authzenBatchAnswer
	if err := json.Unmarshal(body, &batch); err != nil || len(batch.Evaluations) != len(reqs) {
		metrics.RecordAuthorizerRequest(authorizerMetricLabel, op, "error", time.Since(started))
		return nil, fmt.Errorf("%w: invalid evaluations response", ErrPDPUnavailable)
	}
	decisions := make([]bool, len(reqs))
	for i, e := range batch.Evaluations {
		if e.Decision == nil {
			metrics.RecordAuthorizerRequest(authorizerMetricLabel, op, "error", time.Since(started))
			return nil, fmt.Errorf("%w: missing decision in evaluations response", ErrPDPUnavailable)
		}
		decisions[i] = *e.Decision
	}
	metrics.RecordAuthorizerRequest(authorizerMetricLabel, op, "ok", time.Since(started))
	return decisions, nil
}

// BuildNodeAuthzenRequest builds the OpenID AuthZEN 1.0 request for a member
// acting on a node, populating subject.properties (principal, root_id) and
// resource.properties (root_id, <kind>_id entries, and nodes) to match
// gateway's wire contract.
func BuildNodeAuthzenRequest(memberID, principal, rootID, action, kind, nodeID string, chain []ChainNode) AuthzenRequest {
	subProps := map[string]any{"root_id": rootID}
	if principal != "" {
		subProps["principal"] = principal
	}
	resProps := map[string]any{"root_id": rootID}
	if len(chain) > 0 {
		chainNodes := make([]map[string]string, 0, len(chain))
		for _, cn := range chain {
			resProps[cn.Kind+"_id"] = cn.ID
			chainNodes = append(chainNodes, map[string]string{"kind": cn.Kind, "id": cn.ID})
		}
		resProps["nodes"] = chainNodes
	}
	return AuthzenRequest{
		Subject: AuthzenSubject{
			Type:       "member",
			ID:         memberID,
			Properties: subProps,
		},
		Action: AuthzenAction{Name: action},
		Resource: AuthzenResource{
			Type:       kind,
			ID:         nodeID,
			Properties: resProps,
		},
	}
}

// EvaluateAuthorizerBatch evaluates reqs via snap's deployment-wide authorizer,
// mapping unavailable/unconfigured authorizers to 503 SERVICE_UNAVAILABLE.
func EvaluateAuthorizerBatch(ctx context.Context, snap *feed.Snapshot, pdp PDP, reqs []AuthzenRequest) ([]bool, error) {
	if len(reqs) == 0 {
		return nil, nil
	}
	if snap == nil || snap.Authorizer == nil || pdp == nil {
		httpx.LogError(ctx, "external authorizer not configured", ErrPDPUnavailable, errors.KindNetwork)
		return nil, errors.ServiceUnavailableError()
	}
	decisions, err := pdp.EvaluateBatch(ctx, *snap.Authorizer, reqs)
	if err != nil {
		if stderrors.Is(err, ErrPDPUnavailable) {
			httpx.LogError(ctx, "external authorizer evaluation failed", err, errors.KindNetwork)
			return nil, errors.ServiceUnavailableError()
		}
		return nil, err
	}
	return decisions, nil
}
