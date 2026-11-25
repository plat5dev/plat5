# Roles document

What each role grants and where it can be bound. Concept: [access](../../concepts/access.md#roles-and-bindings).

Applied with the rest of the config ([`concepts/config.md`](../../concepts/config.md#plan-and-apply)). Identity follows each change without a restart.

```yaml
roles:
  admin:
    permissions: ["*"]
    bindable_at: [organization, project, environment]
  member:
    permissions: [organization:read]
    bindable_at: [organization]
  developer:
    includes: [member]
    permissions: [project:read, project:write, project:bindings:read]
    bindable_at: [organization, project, environment]
  viewer:
    includes: [member]
    permissions: [project:read]
    bindable_at: [organization, project, environment]
creator_role: admin
```

An example role set for organization > project > environment, where an environment's routes require `project:*` permissions and its `list_permission` is `project:read` ([kinds](kinds.md)). It's honest about grants: whoever holds `bindings:write` can grant any role, so the one role that holds it is `admin`, with `*`. `admin` also shapes the tree, creating and deleting projects and environments; `developer` and `viewer` work inside it.

<!-- generated from schema: roles.json#/properties/roles roles.json#/$defs/role_slug roles.json#/$defs/role/properties roles.json#/properties/creator_role roles.json#/properties/custom_roles roles.json#/properties/permissions -->
| Field | Rule |
|-------|------|
| `roles` | Map of slug → role. At least one role |
| slug | `[a-z][a-z0-9:._-]*`, max 64. Starts with a letter so predefined slugs never collide with custom role ULIDs. Opaque to Plat5 |
| `permissions` | `[a-z0-9:._-]+`, max 64 chars, unique, max 64 permissions. `["*"]` grants every permission; `*` is valid only alone |
| `includes` | Other roles whose permissions this one grants too, transitively. Only permissions are included: `bindable_at` stays the role's own |
| `bindable_at` | Required. The kinds this role can be bound at |
| `creator_role` | Required. What the creator of a node gets on it, where its kind binds its creator (`bind_creator`). Bindable at every kind |
| `custom_roles` | Optional. Enables root-scoped custom roles managed through identity's API alongside predefined roles. Default `false` |
| `permissions` | Optional map of permission → metadata (`description`, `bindable_at`, `custom_role_assignable`) augmenting the permission catalog derived from routes and kinds for custom roles |
<!-- end generated -->

A role grants its own `permissions` plus those of every role it includes. An included role that isn't in `roles`, or a cycle of includes, is **422**. A role may grant any permission, whether or not a route requires it: a service can check a permission no route names through identity's [evaluation API](../api/internal.md#access-evaluation), and a misspelt one grants nothing.

Joining a root takes a role stated on the add or the invite, so default-deny below the root is the role you give new members: with `member` above, org members see no projects until a role is bound at a project, or at the org for all of them.

The rules that tie roles to kinds and routes are checked at apply ([`concepts/config.md`](../../concepts/config.md#validation-at-apply)). Removing a role, or dropping a kind from its `bindable_at`, is safe: bindings keep the name and grant nothing, and grant again if a role with that name comes back. Don't reuse a removed role's name for something else.

When `custom_roles: true`, each root can also define its own ULID-keyed custom roles through identity's API ([`reference/api/identity.md`](../api/identity.md#roles)). Custom roles coexist with predefined roles and can hold any assignable permission from the catalog (never `*`). Use `permissions` to add descriptions, restrict `bindable_at` kinds, or exclude specific permissions from custom roles with `custom_role_assignable: false`.
