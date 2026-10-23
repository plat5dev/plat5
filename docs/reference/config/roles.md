# Roles document

What each role grants and where it can be bound. Concept: [access](../../concepts/access.md#roles-and-bindings).

In `plat5.yml`, applied with the rest of the config ([`concepts/config.md`](../../concepts/config.md#plan-and-apply)). Identity follows each change without a restart.

```yaml
# in plat5.yml
roles:
  admin:     { labels: ["*"], bindable_at: [organization, project] }
  member:    { labels: [organization:read], bindable_at: [organization] }
  developer: { labels: [project:read, catalog:write, subscribers:write], bindable_at: [organization, project] }
  runtime:   { labels: [check], bindable_at: [organization, project] }
  support:   { labels: [project:read, subscribers:write], bindable_at: [organization], max_duration: 7d }
default_role: member
creator_role: admin
```

<!-- generated from schema: roles.json#/properties/roles roles.json#/$defs/role_slug roles.json#/$defs/role/properties roles.json#/properties/default_role roles.json#/properties/creator_role -->
| Field | Rule |
|-------|------|
| `roles` | Map of slug → role. At least one role |
| slug | `[a-z0-9:._-]+`, max 64. Opaque to Plat5 |
| `labels` | `[a-z0-9:._-]+`, max 64 chars, unique, max 64 labels. `[]` grants nothing. `["*"]` grants every label; `*` is valid only alone |
| `bindable_at` | Required. The kinds this role can be bound at. Never the sign-in kind |
| `max_duration` | Optional. A binding with this role must have an `expires_at` no further away than this. A duration: a whole number and `s`, `m`, `h` or `d` (`7d`, `8h`) |
| `default_role` | Required. What joining a root grants. Bindable at every root kind other than the sign-in kind. No `max_duration`: joining without a stated end must grant something that doesn't need one |
| `creator_role` | Required. What the creator of a node gets on it. Bindable at every kind that can be created. No `max_duration` |
<!-- end generated -->

`default_role` sets default-deny or default-allow below the root. With `member` above, org members see no projects until a role is bound at a project, or at the org for all of them.

The cross-document rules are checked with `kinds` and routes at apply ([`concepts/config.md`](../../concepts/config.md#validation-at-apply)). Removing a role, or dropping a kind from its `bindable_at`, is safe: bindings keep the name and grant nothing until it's back.
