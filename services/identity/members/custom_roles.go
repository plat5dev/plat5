package members

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/feed"
	"github.com/plat5dev/plat5/identity/internal/dbx"
	"github.com/plat5dev/plat5/identity/internal/id"
	"github.com/plat5dev/plat5/identity/internal/secret"
	"github.com/plat5dev/plat5/identity/lifecycle"
	"github.com/plat5dev/plat5/identity/nodes"
)

const (
	RoleSourceSystem = "system"
	RoleSourceCustom = "custom"

	maxRoleDescriptionLen = 256
	maxCustomRolePerms    = 64
)

// RoleView is a predefined system role or a root-scoped custom role in one
// unified shape (docs/reference/api/identity.md#roles).
type RoleView struct {
	ID          string
	Slug        string
	DisplayName string
	Description string
	Source      string
	Permissions []string
	BindableAt  []string
	CreatedAt   *time.Time
	UpdatedAt   *time.Time
}

// PermissionView is one entry of GET /nodes/{root_id}/permissions.
type PermissionView struct {
	Name                 string   `json:"name"`
	Description          string   `json:"description"`
	BindableAt           []string `json:"bindable_at"`
	CustomRoleAssignable bool     `json:"custom_role_assignable"`
}

// CustomRoleInput creates an org custom role.
type CustomRoleInput struct {
	DisplayName string
	Description string
	Permissions []string
	BindableAt  []string
}

// CustomRolePatch partially updates an org custom role.
type CustomRolePatch struct {
	DisplayName *string
	Description *string
	Permissions *[]string
	BindableAt  *[]string
}

func normalizeRoleDisplayName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", errors.FieldError("display_name", "Display name is required.")
	}
	if utf8.RuneCountInString(name) > secret.MaxNameLen {
		return "", errors.FieldError("display_name", "Display name is too long.")
	}
	return name, nil
}

func normalizeRoleDescription(raw string) (string, error) {
	desc := strings.TrimSpace(raw)
	if utf8.RuneCountInString(desc) > maxRoleDescriptionLen {
		return "", errors.FieldError("description", "Description is too long.")
	}
	return desc, nil
}

func validateCustomRoleFields(snap *feed.Snapshot, bindableAt, perms []string) ([]string, []string, error) {
	if len(bindableAt) == 0 {
		return nil, nil, errors.FieldError("bindable_at", "Choose at least one kind.")
	}
	cleanKinds := make([]string, 0, len(bindableAt))
	for _, k := range bindableAt {
		if _, ok := snap.Kind(k); !ok {
			return nil, nil, errors.FieldError("bindable_at", fmt.Sprintf("%q is not a kind.", k))
		}
		if !slices.Contains(cleanKinds, k) {
			cleanKinds = append(cleanKinds, k)
		}
	}
	sort.Strings(cleanKinds)

	if len(perms) > maxCustomRolePerms {
		return nil, nil, errors.FieldError("permissions", fmt.Sprintf("At most %d permissions are allowed.", maxCustomRolePerms))
	}
	catalog := snap.PermissionCatalog()
	cleanPerms := make([]string, 0, len(perms))
	for _, p := range perms {
		if p == "*" {
			return nil, nil, errors.FieldError("permissions", "Custom roles cannot grant '*'.")
		}
		meta, ok := catalog[p]
		if !ok {
			return nil, nil, errors.FieldError("permissions", fmt.Sprintf("%q is not a known permission.", p))
		}
		if !meta.CustomRoleAssignable {
			return nil, nil, errors.FieldError("permissions", fmt.Sprintf("%q cannot be assigned to a custom role.", p))
		}
		if len(meta.BindableAt) > 0 {
			for _, k := range cleanKinds {
				if !slices.Contains(meta.BindableAt, k) {
					return nil, nil, errors.FieldError("bindable_at", fmt.Sprintf("Permission %q cannot be bound at %q.", p, k))
				}
			}
		}
		if !slices.Contains(cleanPerms, p) {
			cleanPerms = append(cleanPerms, p)
		}
	}
	sort.Strings(cleanPerms)
	return cleanKinds, cleanPerms, nil
}

func scanCustomRole(row dbx.Scannable) (RoleView, error) {
	var r RoleView
	var createdAt, updatedAt time.Time
	if err := row.Scan(&r.ID, &r.DisplayName, &r.Description, &r.Permissions, &r.BindableAt, &createdAt, &updatedAt); err != nil {
		return RoleView{}, err
	}
	r.Slug = r.ID
	r.Source = RoleSourceCustom
	if r.Permissions == nil {
		r.Permissions = []string{}
	}
	if r.BindableAt == nil {
		r.BindableAt = []string{}
	}
	ct, ut := createdAt.UTC(), updatedAt.UTC()
	r.CreatedAt, r.UpdatedAt = &ct, &ut
	return r, nil
}

func systemRoleView(r feed.Role) RoleView {
	perms := r.Permissions
	if perms == nil {
		perms = []string{}
	}
	bindable := append([]string(nil), r.BindableAt...)
	if bindable == nil {
		bindable = []string{}
	}
	return RoleView{
		ID:          r.Slug,
		Slug:        r.Slug,
		DisplayName: r.Slug,
		Source:      RoleSourceSystem,
		Permissions: perms,
		BindableAt:  bindable,
	}
}

// ListRoles returns system and org custom roles bindable at nodeID's kind (or
// all roles in the tree when all is true on a root node).
func (s *Store) ListRoles(ctx context.Context, snap *feed.Snapshot, nodeID string, all bool) ([]RoleView, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "list_roles", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	n, err := s.nodes.Get(ctx, nodeID)
	if err != nil {
		return nil, op.Reject(err, nodes.ErrNotFound)
	}
	if all && n.ParentID != nil {
		all = false
	}
	out := []RoleView{}
	if snap.ExternalAuthz() {
		op.OK("ok")
		return out, nil
	}
	for _, slug := range snap.RoleSlugs() {
		r := snap.Roles[slug]
		if all || r.BindableAtKind(n.Kind) {
			out = append(out, systemRoleView(r))
		}
	}
	if snap.CustomRoles {
		rows, err := s.pool.Query(ctx, `
			SELECT id, display_name, description, permissions, bindable_at, created_at, updated_at
			FROM org_roles
			WHERE root_id = $1 AND ($2 OR $3 = ANY(bindable_at))
			ORDER BY id ASC
		`, n.RootID, all, n.Kind)
		if err != nil {
			return nil, op.Fail(err)
		}
		customs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (RoleView, error) { return scanCustomRole(r) })
		if err != nil {
			return nil, op.Fail(err)
		}
		out = append(out, customs...)
	}
	op.OK("ok")
	return out, nil
}

// GetRole reads one role by slug (system) or ULID (custom role in nodeID's root).
func (s *Store) GetRole(ctx context.Context, snap *feed.Snapshot, nodeID, roleRef string) (*RoleView, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "get_role", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	n, err := s.nodes.Get(ctx, nodeID)
	if err != nil {
		return nil, op.Reject(err, nodes.ErrNotFound)
	}
	if snap.ExternalAuthz() {
		return nil, op.Expected("not found", ErrNotFound)
	}
	if r, ok := snap.Role(roleRef); ok {
		v := systemRoleView(r)
		op.OK("ok")
		return &v, nil
	}
	if !snap.CustomRoles || !id.Valid(roleRef) {
		return nil, op.Expected("not found", ErrNotFound)
	}
	r, err := scanCustomRole(s.pool.QueryRow(ctx, `
		SELECT id, display_name, description, permissions, bindable_at, created_at, updated_at
		FROM org_roles WHERE id = $1 AND root_id = $2
	`, roleRef, n.RootID))
	if dbx.IsNoRows(err) {
		return nil, op.Expected("not found", ErrNotFound)
	}
	if err != nil {
		return nil, op.Fail(err)
	}
	op.OK("ok")
	return &r, nil
}

func collidesWithSystemRole(snap *feed.Snapshot, displayName string) bool {
	lower := strings.ToLower(displayName)
	for slug := range snap.Roles {
		if strings.ToLower(slug) == lower {
			return true
		}
	}
	return false
}

// CreateCustomRole creates a custom role scoped to rootID.
func (s *Store) CreateCustomRole(ctx context.Context, snap *feed.Snapshot, rootID string, in CustomRoleInput) (*RoleView, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "create_custom_role", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	if snap.ExternalAuthz() {
		return nil, op.Expected("disabled", errBuiltInRBACDisabled())
	}
	if !snap.CustomRoles {
		return nil, op.Expected("disabled", errors.FieldError("custom_roles", "Custom roles are not enabled for this deployment."))
	}
	displayName, err := normalizeRoleDisplayName(in.DisplayName)
	if err != nil {
		return nil, op.Expected("invalid", err)
	}
	if collidesWithSystemRole(snap, displayName) {
		return nil, op.Expected("conflict", errors.ConflictError("A role with that name already exists.", "display_name", displayName))
	}
	desc, err := normalizeRoleDescription(in.Description)
	if err != nil {
		return nil, op.Expected("invalid", err)
	}
	bindableAt, perms, err := validateCustomRoleFields(snap, in.BindableAt, in.Permissions)
	if err != nil {
		return nil, op.Expected("invalid", err)
	}

	w, err := s.begin(ctx)
	if err != nil {
		return nil, op.Fail(err)
	}
	defer w.rollback(ctx)
	root, err := s.lockedRoot(ctx, w, rootID)
	if err != nil {
		return nil, op.Reject(err, nodes.ErrNotFound)
	}
	if err := nodes.Writable(root); err != nil {
		return nil, op.Reject(err)
	}
	now := s.now().UTC()
	roleID := id.New()
	r, err := scanCustomRole(w.tx.QueryRow(ctx, `
		INSERT INTO org_roles (id, root_id, display_name, description, permissions, bindable_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
		RETURNING id, display_name, description, permissions, bindable_at, created_at, updated_at
	`, roleID, root.ID, displayName, desc, perms, bindableAt, now))
	if dbx.IsUniqueViolation(err) {
		return nil, op.Expected("conflict", errors.ConflictError("A role with that name already exists.", "display_name", displayName))
	}
	if err != nil {
		return nil, op.Fail(err)
	}
	if err := w.commit(ctx); err != nil {
		return nil, op.Fail(err)
	}
	op.OK("created")
	return &r, nil
}

// PatchCustomRole updates a root's custom role and emits a root invalidation
// when authz-relevant fields (permissions or bindable_at) change.
func (s *Store) PatchCustomRole(ctx context.Context, snap *feed.Snapshot, rootID, roleID string, patch CustomRolePatch) (*RoleView, *RoleView, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "patch_custom_role", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	if snap.ExternalAuthz() {
		return nil, nil, op.Expected("disabled", errBuiltInRBACDisabled())
	}
	if !snap.CustomRoles {
		return nil, nil, op.Expected("disabled", errors.FieldError("custom_roles", "Custom roles are not enabled for this deployment."))
	}
	if patch.DisplayName == nil && patch.Description == nil && patch.Permissions == nil && patch.BindableAt == nil {
		return nil, nil, op.Expected("invalid", errors.FieldError("", "Nothing to update."))
	}
	if _, isSystem := snap.Role(roleID); isSystem {
		return nil, nil, op.Expected("system role", errors.FieldError("role_id", "System roles cannot be edited."))
	}
	if !id.Valid(roleID) {
		return nil, nil, op.Expected("not found", ErrNotFound)
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
	before, err := scanCustomRole(w.tx.QueryRow(ctx, `
		SELECT id, display_name, description, permissions, bindable_at, created_at, updated_at
		FROM org_roles WHERE id = $1 AND root_id = $2 FOR UPDATE
	`, roleID, root.ID))
	if dbx.IsNoRows(err) {
		return nil, nil, op.Expected("not found", ErrNotFound)
	}
	if err != nil {
		return nil, nil, op.Fail(err)
	}

	displayName := before.DisplayName
	if patch.DisplayName != nil {
		if displayName, err = normalizeRoleDisplayName(*patch.DisplayName); err != nil {
			return nil, nil, op.Expected("invalid", err)
		}
		if collidesWithSystemRole(snap, displayName) {
			return nil, nil, op.Expected("conflict", errors.ConflictError("A role with that name already exists.", "display_name", displayName))
		}
	}
	desc := before.Description
	if patch.Description != nil {
		if desc, err = normalizeRoleDescription(*patch.Description); err != nil {
			return nil, nil, op.Expected("invalid", err)
		}
	}
	bindableAt := before.BindableAt
	if patch.BindableAt != nil {
		bindableAt = *patch.BindableAt
	}
	perms := before.Permissions
	if patch.Permissions != nil {
		perms = *patch.Permissions
	}
	if patch.BindableAt != nil || patch.Permissions != nil {
		if bindableAt, perms, err = validateCustomRoleFields(snap, bindableAt, perms); err != nil {
			return nil, nil, op.Expected("invalid", err)
		}
	}

	changed := before.DisplayName != displayName ||
		before.Description != desc ||
		!slices.Equal(before.Permissions, perms) ||
		!slices.Equal(before.BindableAt, bindableAt)
	after, err := scanCustomRole(w.tx.QueryRow(ctx, `
		UPDATE org_roles
		SET display_name = $2, description = $3, permissions = $4, bindable_at = $5,
		    updated_at = CASE WHEN $6 THEN $7 ELSE updated_at END
		WHERE id = $1
		RETURNING id, display_name, description, permissions, bindable_at, created_at, updated_at
	`, before.ID, displayName, desc, perms, bindableAt, changed, s.now().UTC()))
	if dbx.IsUniqueViolation(err) {
		return nil, nil, op.Expected("conflict", errors.ConflictError("A role with that name already exists.", "display_name", displayName))
	}
	if err != nil {
		return nil, nil, op.Fail(err)
	}
	if !slices.Equal(before.Permissions, after.Permissions) || !slices.Equal(before.BindableAt, after.BindableAt) {
		w.invalidate(lifecycle.InvalidateRoot(root.ID))
	}
	if err := w.commit(ctx); err != nil {
		return nil, nil, op.Fail(err)
	}
	op.OK("ok")
	return &before, &after, nil
}

// DeleteCustomRole deletes a root's custom role and any bindings referencing it
// in that tree. A person's member left with no bindings or groups is removed.
func (s *Store) DeleteCustomRole(ctx context.Context, snap *feed.Snapshot, rootID, roleID string) (*RoleView, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "delete_custom_role", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	if snap.ExternalAuthz() {
		return nil, op.Expected("disabled", errBuiltInRBACDisabled())
	}

	if _, isSystem := snap.Role(roleID); isSystem {
		return nil, op.Expected("system role", errors.FieldError("role_id", "System roles cannot be deleted."))
	}
	w, err := s.begin(ctx)
	if err != nil {
		return nil, op.Fail(err)
	}
	defer w.rollback(ctx)
	root, err := s.lockedRoot(ctx, w, rootID)
	if err != nil {
		return nil, op.Reject(err, nodes.ErrNotFound)
	}
	if !id.Valid(roleID) {
		op.OK("unchanged")
		return nil, nil
	}
	before, err := scanCustomRole(w.tx.QueryRow(ctx, `
		SELECT id, display_name, description, permissions, bindable_at, created_at, updated_at
		FROM org_roles WHERE id = $1 AND root_id = $2 FOR UPDATE
	`, roleID, root.ID))
	if dbx.IsNoRows(err) {
		op.OK("unchanged")
		return nil, nil
	}
	if err != nil {
		return nil, op.Fail(err)
	}
	rows, err := w.tx.Query(ctx, `
		WITH deleted AS (
			DELETE FROM bindings b
			USING nodes n
			WHERE b.node_id = n.id AND n.root_id = $1 AND b.role = $2
			RETURNING b.member_id
		)
		SELECT DISTINCT member_id FROM deleted WHERE member_id IS NOT NULL
	`, root.ID, before.ID)
	if err != nil {
		return nil, op.Fail(err)
	}
	touched, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, op.Fail(err)
	}
	if _, err := w.tx.Exec(ctx, `DELETE FROM org_roles WHERE id = $1`, before.ID); err != nil {
		return nil, op.Fail(err)
	}
	if err := s.removeEmptyPeople(ctx, w, touched); err != nil {
		return nil, op.Fail(err)
	}
	w.invalidate(lifecycle.InvalidateRoot(root.ID))
	if err := w.commit(ctx); err != nil {
		return nil, op.Fail(err)
	}
	op.OK("deleted")
	return &before, nil
}

// ListPermissions returns the deployment's permission catalog for rootID.
func (s *Store) ListPermissions(ctx context.Context, snap *feed.Snapshot, rootID string) ([]PermissionView, error) {
	ctx, cancel, op := dbx.BeginTimeout(ctx, s.tracer, "list_permissions", dbx.DefaultTimeout)
	defer cancel()
	defer op.End()

	if err := s.checkRoot(ctx, rootID); err != nil {
		return nil, op.Reject(err, nodes.ErrNotFound)
	}
	if snap.ExternalAuthz() {
		op.OK("ok")
		return []PermissionView{}, nil
	}
	catalog := snap.PermissionCatalog()
	names := make([]string, 0, len(catalog))
	for name := range catalog {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]PermissionView, 0, len(names))
	for _, name := range names {
		meta := catalog[name]
		bindable := append([]string(nil), meta.BindableAt...)
		if bindable == nil {
			bindable = []string{}
		}
		out = append(out, PermissionView{
			Name:                 name,
			Description:          meta.Description,
			BindableAt:           bindable,
			CustomRoleAssignable: meta.CustomRoleAssignable,
		})
	}
	op.OK("ok")
	return out, nil
}
