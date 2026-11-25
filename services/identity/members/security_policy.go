package members

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/dbx"
	"github.com/plat5dev/plat5/identity/lifecycle"
	"github.com/plat5dev/plat5/identity/nodes"
)

const (
	maxPolicyClaimKeys   = 16
	maxPolicyClaimValues = 32
	maxPolicyClaimValLen = 256
)

var policyClaimKeyRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// RootSecurityPolicy is a root tree's authentication and credential policy
// (docs/reference/api/identity.md#security-policy).
type RootSecurityPolicy struct {
	AllowPersonalKeys bool                `json:"allow_personal_keys"`
	AllowedCIDRs      []string            `json:"allowed_cidrs"`
	RequiredIdPClaims map[string][]string `json:"required_idp_claims"`
	UpdatedAt         *time.Time          `json:"-"`
}

// SecurityPolicyPatch is a partial update to a root's security policy.
type SecurityPolicyPatch struct {
	AllowPersonalKeys    *bool
	AllowedCIDRsSet      bool
	AllowedCIDRs         []string
	RequiredIdPClaimsSet bool
	RequiredIdPClaims    map[string][]string
}

func defaultSecurityPolicy() *RootSecurityPolicy {
	return &RootSecurityPolicy{
		AllowPersonalKeys: true,
		AllowedCIDRs:      []string{},
		RequiredIdPClaims: map[string][]string{},
	}
}

func normalizeRequiredIdPClaims(raw map[string][]string) (map[string][]string, error) {
	out := map[string][]string{}
	if len(raw) == 0 {
		return out, nil
	}
	if len(raw) > maxPolicyClaimKeys {
		return nil, errors.FieldError("required_idp_claims", "At most 16 claim rules are allowed.")
	}
	for k, vals := range raw {
		key := strings.TrimSpace(k)
		if !policyClaimKeyRe.MatchString(key) {
			return nil, errors.FieldError("required_idp_claims", "Claim names must be 1 to 64 of letters, digits, '.', '_' or '-'.")
		}
		if len(vals) == 0 || len(vals) > maxPolicyClaimValues {
			return nil, errors.FieldError("required_idp_claims."+key, "Each claim rule must list 1 to 32 allowed values.")
		}
		clean := make([]string, 0, len(vals))
		for _, v := range vals {
			s := strings.TrimSpace(v)
			if s == "" || len(s) > maxPolicyClaimValLen {
				return nil, errors.FieldError("required_idp_claims."+key, "Claim values must be non-empty strings up to 256 bytes.")
			}
			if !slices.Contains(clean, s) {
				clean = append(clean, s)
			}
		}
		slices.Sort(clean)
		out[key] = clean
	}
	return out, nil
}

func readSecurityPolicy(ctx context.Context, q dbx.Querier, rootID string) (*RootSecurityPolicy, error) {
	var allowPersonal bool
	var cidrs []string
	var claimsRaw []byte
	var updatedAt time.Time
	err := q.QueryRow(ctx, `
		SELECT allow_personal_keys, allowed_cidrs, required_idp_claims, updated_at
		FROM root_security_policies WHERE root_id = $1
	`, rootID).Scan(&allowPersonal, &cidrs, &claimsRaw, &updatedAt)
	if dbx.IsNoRows(err) {
		return defaultSecurityPolicy(), nil
	}
	if err != nil {
		return nil, err
	}
	if cidrs == nil {
		cidrs = []string{}
	}
	claims := map[string][]string{}
	if len(claimsRaw) > 0 {
		if err := json.Unmarshal(claimsRaw, &claims); err != nil {
			return nil, err
		}
	}
	if claims == nil {
		claims = map[string][]string{}
	}
	t := updatedAt.UTC()
	return &RootSecurityPolicy{
		AllowPersonalKeys: allowPersonal,
		AllowedCIDRs:      cidrs,
		RequiredIdPClaims: claims,
		UpdatedAt:         &t,
	}, nil
}

// GetSecurityPolicy returns a root node's security policy, or defaults if unset.
func (s *Store) GetSecurityPolicy(ctx context.Context, rootID string) (*RootSecurityPolicy, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "get_security_policy", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	if err := s.checkRoot(ctx, rootID); err != nil {
		return nil, op.Reject(err, nodes.ErrNotFound)
	}
	pol, err := readSecurityPolicy(ctx, s.pool, rootID)
	if err != nil {
		return nil, op.Fail(err)
	}
	op.OK("ok")
	return pol, nil
}

func claimMapsEqual(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || !slices.Equal(av, bv) {
			return false
		}
	}
	return true
}

// PatchSecurityPolicy updates a root node's security policy and emits a root
// admit invalidation when changed.
func (s *Store) PatchSecurityPolicy(ctx context.Context, rootID string, patch SecurityPolicyPatch) (*RootSecurityPolicy, *RootSecurityPolicy, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "patch_security_policy", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	if patch.AllowPersonalKeys == nil && !patch.AllowedCIDRsSet && !patch.RequiredIdPClaimsSet {
		return nil, nil, op.Expected("invalid", errors.FieldError("", "Nothing to update."))
	}
	var cidrs []string
	if patch.AllowedCIDRsSet {
		var err error
		cidrs, err = normalizeAllowedCIDRs(patch.AllowedCIDRs)
		if err != nil {
			return nil, nil, op.Expected("invalid", err)
		}
	}
	var claims map[string][]string
	if patch.RequiredIdPClaimsSet {
		var err error
		claims, err = normalizeRequiredIdPClaims(patch.RequiredIdPClaims)
		if err != nil {
			return nil, nil, op.Expected("invalid", err)
		}
	}

	w, err := s.begin(ctx)
	if err != nil {
		return nil, nil, op.Fail(err)
	}
	defer w.rollback(ctx)
	root, err := s.lockedRoot(ctx, w, rootID)
	if err != nil {
		return nil, nil, op.Reject(err, nodes.ErrNotFound)
	}
	if err := nodes.Writable(root); err != nil {
		return nil, nil, op.Reject(err)
	}

	before, err := readSecurityPolicy(ctx, w.tx, root.ID)
	if err != nil {
		return nil, nil, op.Fail(err)
	}
	next := &RootSecurityPolicy{
		AllowPersonalKeys: before.AllowPersonalKeys,
		AllowedCIDRs:      slices.Clone(before.AllowedCIDRs),
		RequiredIdPClaims: before.RequiredIdPClaims,
	}
	if patch.AllowPersonalKeys != nil {
		next.AllowPersonalKeys = *patch.AllowPersonalKeys
	}
	if patch.AllowedCIDRsSet {
		next.AllowedCIDRs = cidrs
	}
	if patch.RequiredIdPClaimsSet {
		next.RequiredIdPClaims = claims
	}

	claimsJSON, err := json.Marshal(next.RequiredIdPClaims)
	if err != nil {
		return nil, nil, op.Fail(err)
	}
	now := s.now().UTC()
	if _, err := w.tx.Exec(ctx, `
		INSERT INTO root_security_policies (root_id, allow_personal_keys, allowed_cidrs, required_idp_claims, updated_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (root_id) DO UPDATE SET
			allow_personal_keys = EXCLUDED.allow_personal_keys,
			allowed_cidrs       = EXCLUDED.allowed_cidrs,
			required_idp_claims = EXCLUDED.required_idp_claims,
			updated_at          = EXCLUDED.updated_at
	`, root.ID, next.AllowPersonalKeys, next.AllowedCIDRs, claimsJSON, now); err != nil {
		return nil, nil, op.Fail(err)
	}
	next.UpdatedAt = &now
	changed := before.AllowPersonalKeys != next.AllowPersonalKeys ||
		!slices.Equal(before.AllowedCIDRs, next.AllowedCIDRs) ||
		!claimMapsEqual(before.RequiredIdPClaims, next.RequiredIdPClaims)
	if changed {
		w.invalidate(lifecycle.InvalidateRoot(root.ID))
	}
	if err := w.commit(ctx); err != nil {
		return nil, nil, op.Fail(err)
	}
	op.OK("ok")
	return before, next, nil
}
