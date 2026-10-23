# Roles and bindings

A deployment's roles, and how members hold them. Plat5 ships no role names: the deployment's roles document names each role, the labels it grants, and the kinds it can be bound at. A **binding** puts one role on one member at one node. What a member may call on a node is the union of its bindings on that node and its ancestors.

Model: [`model.md`](model.md). Identity API: [`identity.md`](identity.md). Route labels: [`routes.md`](routes.md).

## The model

| Piece | Says | Owner |
|-------|------|-------|
| Route `required_labels` | Which labels the route needs (any one of them) | The service's `routes.yml` |
| Role | Which labels it grants, and where it can be bound | The deployment's roles document |
| Binding | This member holds this role on this node, until `expires_at` | Identity, written through the node API |

The gateway checks the caller's labels on the route's node against the route. It never sees roles or bindings, only labels, from admit. Services see none of it.

Roles are always on. There's no deployment-wide switch: a deployment where something else decides access gives its roles `["*"]`.

A credential carries its principal's bindings in the tree it's used in. Keys have no labels of their own. To give a machine less, give it a service account with fewer or narrower bindings.

## The roles document

It's a document in the deployment's config, applied with the rest of it ([`control-plane.md`](control-plane.md#plan-and-apply)). Identity follows each change without a restart.

```yaml
version: 2
roles:
  admin:     { labels: ["*"], bindable_at: [organization, project] }
  member:    { labels: [organization:read], bindable_at: [organization] }
  developer: { labels: [project:read, catalog:write, subscribers:write], bindable_at: [organization, project] }
  runtime:   { labels: [check], bindable_at: [organization, project] }
  support:   { labels: [project:read, subscribers:write], bindable_at: [organization], max_duration: 7d }
default_role: member
creator_role: admin
```

| Field | Rule |
|-------|------|
| `roles` | Map of slug → role. At least one role |
| slug | `[a-z0-9:._-]+`, max 64. Opaque to Plat5 |
| `labels` | `[a-z0-9:._-]+`, max 64 chars, unique, max 64 labels. `[]` grants nothing. `["*"]` grants every label; `*` is valid only alone |
| `bindable_at` | Required. The kinds this role can be bound at. Never the sign-in kind |
| `max_duration` | Optional. A binding with this role must have an `expires_at` no further away than this |
| `default_role` | Required. What joining a root grants. Bindable at every root kind other than the sign-in kind |
| `creator_role` | Required. What the creator of a node gets on it. Bindable at every kind that can be created |

`default_role` sets default-deny or default-allow below the root. With `member` above, org members see no projects until a role is bound at a project, or at the org for all of them.

The cross-document rules are checked with `kinds` and routes at apply ([`control-plane.md`](control-plane.md#validation-at-apply)). Removing a role, or dropping a kind from its `bindable_at`, is safe: bindings keep the name and grant nothing until it's back.

## Resolution

Identity resolves labels at admit, for one member on one node chain ([`identity.md`](identity.md#admit)).

1. Take the member's bindings on every node in the chain: the route's node and each ancestor.
2. Drop any binding past its `expires_at`, whose role isn't in the document, or whose role isn't bindable at its node's kind.
3. `labels` is the union of the remaining roles' labels.

`*` is every label: the gateway matches it to any route label. A route can't require `*` itself.

Admit also returns `good_until`, the earliest `expires_at` among the bindings that contributed, so the gateway's cache never outlives an expiring grant. Any other change (a role edit, a removed binding) reaches the edge within `APIKEY_CACHE_TTL_SECS`.

## Assigning

| Write | Role |
|-------|------|
| Create a node | The creator gets `creator_role` on it, unless an inherited binding already holds `creator_role`. A node created with no member behind the request gets no binding |
| Add a member to a root | `role` in the body, or `default_role` |
| Redeem an invite at a root | The invite's `role` at the root, plus each of its `grants` |
| Redeem an invite below a root | The invite's `role` at that node. For someone not in the tree, that makes them a guest |
| Create a service account | `role` in the body, required. Bound at the node it's born at |
| Create or change a binding | `role` in the body, required |

A role not in the document, or not bindable at the node's kind, is **422**. An `expires_at` past the role's `max_duration`, or missing when the role has one, is **422**. A direct binding for someone who isn't in the tree is **422**: guests come through invites.

One binding per member per node. Changing a member's role on a node is a `PATCH` of that binding.

## Who may assign

There is no grant cap. Identity doesn't compare the caller to the role it assigns, or to the member it acts on. Whoever may call a binding route may bind any role bindable there, including one with more labels than its own. The route's labels decide who may call; that's the gateway's check.

A service account belongs to its tree, not to whoever created it. Its bindings are its own, and its keys carry them.

So the labels that reach binding routes are as strong as the strongest role. Give `<kind>:bindings:write` only to members you'd trust with every role bindable at that kind.

## Listing roles

`GET /orgs/{organization}/roles`, and the same at each kind, returns the roles bindable there, for role pickers.

```json
{
  "roles": [
    { "slug": "admin", "labels": ["*"], "max_duration": null },
    { "slug": "developer", "labels": ["project:read", "catalog:write", "subscribers:write"], "max_duration": null },
    { "slug": "runtime", "labels": ["check"], "max_duration": null }
  ],
  "default_role": "member",
  "creator_role": "admin"
}
```

Sorted by slug. Not paginated: the document is the whole list. `default_role` is `null` below a root.

## Generated labels

The node routes identity generates carry a fixed set of labels per kind (`<kind>:read`, `<kind>:create`, `<kind>:bindings:write`, …). They're catalog defaults, not rules: the team renames or drops them in the applied catalog, and Plat5's code depends on no label name. The table: [`identity.md`](identity.md#generated-labels).

## Not here

- Deny rules, or any way a binding takes labels away
- Key labels, or any other way to narrow a credential below its principal
- A grant cap: comparing the caller to the role it assigns
- Telling services the caller's role, bindings or labels
- Role names or meanings in Plat5 code
- Roles in the gateway. The control plane stores the document; only identity reads it
- More than one binding per member per node
- Per-tree custom roles (deferred, [`../TODO.md`](../TODO.md))
- Permissions on records inside a node (the deferred policy hook)
