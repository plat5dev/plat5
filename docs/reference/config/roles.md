# Roles document

What each role grants and where it can be bound. Concept: [access](../../concepts/access.md#roles-and-bindings).

In `plat5.yml`, applied with the rest of the config ([`concepts/config.md`](../../concepts/config.md#plan-and-apply)). Identity follows each change without a restart.

```yaml
# in plat5.yml
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

<!-- generated from schema: roles.json#/properties/roles roles.json#/$defs/role_slug roles.json#/$defs/role/properties roles.json#/properties/creator_role -->
| Field | Rule |
|-------|------|
| `roles` | Map of slug → role. At least one role |
| slug | `[a-z0-9:._-]+`, max 64. Opaque to Plat5 |
| `permissions` | `[a-z0-9:._-]+`, max 64 chars, unique, max 64 permissions. `["*"]` grants every permission; `*` is valid only alone |
| `includes` | Other roles whose permissions this one grants too, transitively. Only permissions are included: `bindable_at` stays the role's own |
| `bindable_at` | Required. The kinds this role can be bound at |
| `creator_role` | Required. What the creator of a node gets on it, where its kind binds its creator (`bind_creator`). Bindable at every kind |
<!-- end generated -->

A role grants its own `permissions` plus those of every role it includes. An included role that isn't in `roles`, or a cycle of includes, is **422**. A role may grant any permission, whether or not a route requires it: a service can check a permission no route names through identity's [evaluation API](../api/internal.md#access-evaluation), and a misspelt one grants nothing.

Joining a root takes a role stated on the add or the invite, so default-deny below the root is the role you give new members: with `member` above, org members see no projects until a role is bound at a project, or at the org for all of them.

The rules that tie roles to kinds and routes are checked at apply ([`concepts/config.md`](../../concepts/config.md#validation-at-apply)). Removing a role, or dropping a kind from its `bindable_at`, is safe: bindings keep the name and grant nothing, and grant again if a role with that name comes back. Don't reuse a removed role's name for something else.
