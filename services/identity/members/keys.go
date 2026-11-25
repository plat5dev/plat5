package members

import (
	"context"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/internal/dbx"
	"github.com/plat5dev/plat5/identity/internal/id"
	"github.com/plat5dev/plat5/identity/internal/secret"
	"github.com/plat5dev/plat5/identity/lifecycle"
	"github.com/plat5dev/plat5/identity/nodes"
)

const (
	// MaxAllowedCIDRs is the most entries an API key's IP allowlist may hold.
	MaxAllowedCIDRs = 50
	// MaxKeyScopes is the most permission scopes an API key may hold.
	MaxKeyScopes = 64
	// MaxAllowedRoots is the most root node IDs a personal API key may allow.
	MaxAllowedRoots = 50
)

var scopePermRe = regexp.MustCompile(`^[a-z0-9:._-]{1,64}$`)

// Key is an API key, on a person or a service account.
type Key struct {
	ID           string
	Name         string
	KeyPrefix    string
	AllowedCIDRs []string
	Scopes       []string
	AllowedRoots []string
	ExpiresAt    *time.Time
	CreatedAt    time.Time
	RevokedAt    *time.Time
	// Secret is set once, on create.
	Secret string
}

// KeyCreate is what POST .../keys accepts.
type KeyCreate struct {
	Name         string
	AllowedCIDRs []string
	Scopes       []string
	AllowedRoots []string
	ExpiresAt    *time.Time
}

// KeyPatch is what PATCH .../keys/{key_id} changes. A nil field is left alone.
type KeyPatch struct {
	Name         *string
	AllowedCIDRs *[]string
	Scopes       *[]string
	AllowedRoots *[]string
	ExpiresAtSet bool
	ExpiresAt    *time.Time
}

const keyColumns = `id, name, key_prefix, allowed_cidrs, scopes, allowed_roots, expires_at, created_at, revoked_at`

func scanKey(row dbx.Scannable) (*Key, error) {
	var k Key
	if err := row.Scan(&k.ID, &k.Name, &k.KeyPrefix, &k.AllowedCIDRs, &k.Scopes, &k.AllowedRoots, &k.ExpiresAt, &k.CreatedAt, &k.RevokedAt); err != nil {
		return nil, err
	}
	normalizeKeySlices(&k)
	return &k, nil
}

func normalizeKeySlices(k *Key) {
	if k.AllowedCIDRs == nil {
		k.AllowedCIDRs = []string{}
	}
	if k.Scopes == nil {
		k.Scopes = []string{}
	}
	if k.AllowedRoots == nil {
		k.AllowedRoots = []string{}
	}
}

// normalizeScopes validates, deduplicates, and sorts a key's permission scopes.
// Empty means unrestricted; "*" is rejected so callers omit scopes instead.
func normalizeScopes(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return []string{}, nil
	}
	if len(raw) > MaxKeyScopes {
		return nil, errors.FieldError("scopes", fmt.Sprintf("At most %d scopes are allowed.", MaxKeyScopes))
	}
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for i, entry := range raw {
		s := strings.TrimSpace(entry)
		if s == "*" {
			return nil, errors.FieldError(fmt.Sprintf("scopes[%d]", i), "Omit scopes or send [] for unrestricted access.")
		}
		if !scopePermRe.MatchString(s) {
			return nil, errors.FieldError(fmt.Sprintf("scopes[%d]", i), "Must be a valid permission name.")
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	slices.Sort(out)
	return out, nil
}

// normalizeAllowedRoots validates, deduplicates, and sorts a personal key's
// root node ID allowlist. Service account keys cannot set allowed_roots.
func normalizeAllowedRoots(raw []string, isServiceAccount bool) ([]string, error) {
	if len(raw) == 0 {
		return []string{}, nil
	}
	if isServiceAccount {
		return nil, errors.FieldError("allowed_roots", "Service account keys are already scoped to their own tree.")
	}
	if len(raw) > MaxAllowedRoots {
		return nil, errors.FieldError("allowed_roots", fmt.Sprintf("At most %d roots are allowed.", MaxAllowedRoots))
	}
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for i, entry := range raw {
		s := strings.TrimSpace(entry)
		if !id.Valid(s) {
			return nil, errors.FieldError(fmt.Sprintf("allowed_roots[%d]", i), "Must be a valid node id.")
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	slices.Sort(out)
	return out, nil
}

// normalizeAllowedCIDRs parses, canonicalizes, deduplicates, and sorts an
// IP/CIDR allowlist. An empty list means unrestricted.
func normalizeAllowedCIDRs(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return []string{}, nil
	}
	if len(raw) > MaxAllowedCIDRs {
		return nil, errors.FieldError("allowed_cidrs", fmt.Sprintf("At most %d entries are allowed.", MaxAllowedCIDRs))
	}
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for i, entry := range raw {
		s := strings.TrimSpace(entry)
		var prefix netip.Prefix
		if strings.Contains(s, "/") {
			p, err := netip.ParsePrefix(s)
			if err != nil || p.Addr().Zone() != "" {
				return nil, errors.FieldError(fmt.Sprintf("allowed_cidrs[%d]", i), "Must be a valid IP address or CIDR block.")
			}
			if p.Addr().Is4In6() && p.Bits() >= 96 {
				prefix = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96).Masked()
			} else {
				prefix = p.Masked()
			}
		} else {
			addr, err := netip.ParseAddr(s)
			if err != nil || addr.Zone() != "" {
				return nil, errors.FieldError(fmt.Sprintf("allowed_cidrs[%d]", i), "Must be a valid IP address or CIDR block.")
			}
			addr = addr.Unmap()
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		canon := prefix.String()
		if _, dup := seen[canon]; dup {
			continue
		}
		seen[canon] = struct{}{}
		out = append(out, canon)
	}
	slices.Sort(out)
	return out, nil
}

// KeyOwner is whose keys a request addresses: a service account that belongs to a
// node, by its member id, or a person, by their user id.
type KeyOwner struct {
	NodeID   string
	MemberID string
	UserID   string
}

func (s *Store) keyPrincipal(ctx context.Context, q dbx.Querier, owner KeyOwner) (string, error) {
	if owner.UserID != "" {
		u, err := s.user(ctx, q, owner.UserID)
		if err != nil {
			return "", err
		}
		return u.principalID, nil
	}
	_, principalID, err := s.serviceAccount(ctx, q, owner.NodeID, owner.MemberID)
	return principalID, err
}

// lockedKeyPrincipal is keyPrincipal for a write: a service account's keys
// are what hangs off its node, so the node is locked and must not be purged.
func (s *Store) lockedKeyPrincipal(ctx context.Context, w *write, owner KeyOwner) (string, error) {
	if owner.UserID == "" {
		if _, err := s.nodes.LockedWritable(ctx, w.tx, owner.NodeID); err != nil {
			return "", err
		}
	}
	return s.keyPrincipal(ctx, w.tx, owner)
}

// ListKeys is the owner's keys, revoked ones included, id ascending.
func (s *Store) ListKeys(ctx context.Context, owner KeyOwner, limit int, startingAfter string) ([]*Key, bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "list_keys", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()
	principalID, err := s.keyPrincipal(ctx, s.pool, owner)
	if err != nil {
		return nil, false, op.Reject(err, ErrNotFound)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+keyColumns+` FROM api_keys WHERE principal_id = $1 AND id > $2 ORDER BY id LIMIT $3
	`, principalID, startingAfter, limit+1)
	if err != nil {
		return nil, false, op.Fail(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Key, error) { return scanKey(r) })
	if err != nil {
		return nil, false, op.Fail(err)
	}
	out, hasMore := dbx.Page(out, limit)
	op.OK("ok")
	return out, hasMore, nil
}

// CreateKey mints a key for the owner. The secret is returned once and stored
// only as its hash.
func (s *Store) CreateKey(ctx context.Context, owner KeyOwner, in KeyCreate) (*Key, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "create_key", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = secret.DefaultName
	}
	if utf8.RuneCountInString(name) > secret.MaxNameLen {
		return nil, op.Expected("invalid", errors.FieldError("name", "Name is too long."))
	}
	cidrs, err := normalizeAllowedCIDRs(in.AllowedCIDRs)
	if err != nil {
		return nil, op.Expected("invalid", err)
	}
	scopes, err := normalizeScopes(in.Scopes)
	if err != nil {
		return nil, op.Expected("invalid", err)
	}
	allowedRoots, err := normalizeAllowedRoots(in.AllowedRoots, owner.UserID == "")
	if err != nil {
		return nil, op.Expected("invalid", err)
	}
	if in.ExpiresAt != nil && !in.ExpiresAt.After(s.now().UTC()) {
		return nil, op.Expected("invalid", errors.FieldError("expires_at", "Expiry must be in the future."))
	}
	w, err := s.begin(ctx)
	if err != nil {
		return nil, op.Fail(err)
	}
	defer w.rollback(ctx)
	principalID, err := s.lockedKeyPrincipal(ctx, w, owner)
	if err != nil {
		return nil, op.Reject(err, ErrNotFound, nodes.ErrNotFound)
	}
	prefix := secret.KeyPrefix(s.brand)
	key, err := secret.Generate(prefix)
	if err != nil {
		return nil, op.Fail(err)
	}
	k, err := scanKey(w.tx.QueryRow(ctx, `
		INSERT INTO api_keys (id, principal_id, name, key_prefix, key_hash, allowed_cidrs, scopes, allowed_roots, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING `+keyColumns,
		id.New(), principalID, name, secret.DisplayPrefix(key, prefix), secret.Hash(key),
		cidrs, scopes, allowedRoots, in.ExpiresAt, s.now().UTC()))
	if dbx.IsForeignKeyViolation(err) {
		return nil, op.Expected("not found", ErrNotFound) // the owner went meanwhile
	}
	if err != nil {
		return nil, op.Fail(err)
	}
	if err := w.commit(ctx); err != nil {
		return nil, op.Fail(err)
	}
	k.Secret = key
	op.OK("created")
	return k, nil
}

// UpdateKey changes a live key's name, IP allowlist, scopes, allowed roots,
// and/or expiry. Editing a revoked key returns 409 KEY_REVOKED.
func (s *Store) UpdateKey(ctx context.Context, owner KeyOwner, keyID string, patch KeyPatch) (*Key, *Key, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "update_key", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()
	if patch.Name == nil && patch.AllowedCIDRs == nil && patch.Scopes == nil && patch.AllowedRoots == nil && !patch.ExpiresAtSet {
		return nil, nil, op.Expected("invalid", errors.FieldError("", "Nothing to update."))
	}
	var newName *string
	if patch.Name != nil {
		n, err := normalizeName(*patch.Name)
		if err != nil {
			return nil, nil, op.Expected("invalid", err)
		}
		newName = &n
	}
	var newCIDRs []string
	if patch.AllowedCIDRs != nil {
		cidrs, err := normalizeAllowedCIDRs(*patch.AllowedCIDRs)
		if err != nil {
			return nil, nil, op.Expected("invalid", err)
		}
		newCIDRs = cidrs
	}
	var newScopes []string
	if patch.Scopes != nil {
		scopes, err := normalizeScopes(*patch.Scopes)
		if err != nil {
			return nil, nil, op.Expected("invalid", err)
		}
		newScopes = scopes
	}
	var newAllowedRoots []string
	if patch.AllowedRoots != nil {
		roots, err := normalizeAllowedRoots(*patch.AllowedRoots, owner.UserID == "")
		if err != nil {
			return nil, nil, op.Expected("invalid", err)
		}
		newAllowedRoots = roots
	}
	if patch.ExpiresAtSet && patch.ExpiresAt != nil && !patch.ExpiresAt.After(s.now().UTC()) {
		return nil, nil, op.Expected("invalid", errors.FieldError("expires_at", "Expiry must be in the future."))
	}
	w, err := s.begin(ctx)
	if err != nil {
		return nil, nil, op.Fail(err)
	}
	defer w.rollback(ctx)
	principalID, err := s.lockedKeyPrincipal(ctx, w, owner)
	if err == nil && !id.Valid(keyID) {
		err = ErrNotFound
	}
	if err != nil {
		return nil, nil, op.Reject(err, ErrNotFound, nodes.ErrNotFound)
	}
	before, err := scanKey(w.tx.QueryRow(ctx, `
		SELECT `+keyColumns+` FROM api_keys WHERE id = $1 AND principal_id = $2 FOR UPDATE
	`, keyID, principalID))
	if dbx.IsNoRows(err) {
		return nil, nil, op.Expected("not found", ErrNotFound)
	}
	if err != nil {
		return nil, nil, op.Fail(err)
	}
	if before.RevokedAt != nil {
		return nil, nil, op.Expected("revoked", errors.KeyRevokedError())
	}
	name := before.Name
	if newName != nil {
		name = *newName
	}
	cidrs := before.AllowedCIDRs
	if patch.AllowedCIDRs != nil {
		cidrs = newCIDRs
	}
	scopes := before.Scopes
	if patch.Scopes != nil {
		scopes = newScopes
	}
	allowedRoots := before.AllowedRoots
	if patch.AllowedRoots != nil {
		allowedRoots = newAllowedRoots
	}
	expiresAt := before.ExpiresAt
	if patch.ExpiresAtSet {
		expiresAt = patch.ExpiresAt
	}
	after, err := scanKey(w.tx.QueryRow(ctx, `
		UPDATE api_keys SET name = $3, allowed_cidrs = $4, scopes = $5, allowed_roots = $6, expires_at = $7
		WHERE id = $1 AND principal_id = $2
		RETURNING `+keyColumns, keyID, principalID, name, cidrs, scopes, allowedRoots, expiresAt))
	if err != nil {
		return nil, nil, op.Fail(err)
	}
	if patch.AllowedCIDRs != nil || patch.Scopes != nil || patch.AllowedRoots != nil || patch.ExpiresAtSet {
		w.invalidate(lifecycle.InvalidateKey(keyID))
	}
	if err := w.commit(ctx); err != nil {
		return nil, nil, op.Fail(err)
	}
	op.OK("ok")
	return before, after, nil
}

// RevokeKey revokes one of the owner's keys. Revoking a revoked key changes
// nothing: changed is false.
func (s *Store) RevokeKey(ctx context.Context, owner KeyOwner, keyID string) (*Key, bool, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "revoke_key", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()
	w, err := s.begin(ctx)
	if err != nil {
		return nil, false, op.Fail(err)
	}
	defer w.rollback(ctx)
	principalID, err := s.lockedKeyPrincipal(ctx, w, owner)
	if err == nil && !id.Valid(keyID) {
		err = ErrNotFound
	}
	if err != nil {
		return nil, false, op.Reject(err, ErrNotFound, nodes.ErrNotFound)
	}
	var k Key
	var changed bool
	err = w.tx.QueryRow(ctx, `
		UPDATE api_keys k SET revoked_at = COALESCE(k.revoked_at, $3)
		FROM (SELECT id, revoked_at FROM api_keys WHERE id = $1 AND principal_id = $2 FOR UPDATE) old
		WHERE k.id = old.id
		RETURNING k.id, k.name, k.key_prefix, k.allowed_cidrs, k.scopes, k.allowed_roots, k.expires_at, k.created_at, k.revoked_at, old.revoked_at IS NULL
	`, keyID, principalID, s.now().UTC()).Scan(
		&k.ID, &k.Name, &k.KeyPrefix, &k.AllowedCIDRs, &k.Scopes, &k.AllowedRoots, &k.ExpiresAt, &k.CreatedAt, &k.RevokedAt, &changed,
	)
	if dbx.IsNoRows(err) {
		return nil, false, op.Expected("not found", ErrNotFound)
	}
	if err != nil {
		return nil, false, op.Fail(err)
	}
	normalizeKeySlices(&k)
	if changed {
		w.invalidate(lifecycle.InvalidateKey(k.ID))
	}
	if err := w.commit(ctx); err != nil {
		return nil, false, op.Fail(err)
	}
	op.OK("ok")
	return &k, changed, nil
}
