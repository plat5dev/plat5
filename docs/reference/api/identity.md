# Identity service API

What identity's [generated routes](../config/generated-routes.md) point at, and what any other front door calls. Every path is ids; nothing here resolves slugs. Concepts: [access](../../concepts/access.md), [lifecycle](../../concepts/lifecycle.md).

The service is a function of the URL. Its own API addresses everything by id: `/nodes/{node_id}/…`, `/users/{user_id}/…`. Slugs exist only at the gateway; identity's service file maps public paths onto these.

Who may call is the front door. Identity doesn't know which front door called, and doesn't compare the caller to what it grants. It reads the caller on three routes only:

| Route | Reads | For |
|-------|-------|-----|
| `POST /nodes/{node_id}/children/{kind}` | `X-Plat5-Member-Id` | That member gets `creator_role` on the new node in built-in RBAC mode, if the kind binds its creator |
| `GET /nodes/{node_id}/children/{kind}` | `X-Plat5-Member-Id` | The list is filtered to what that member can read |
| `POST /roots/{kind}` | `X-Plat5-User-Id` | That user gets a member in the new tree (plus `creator_role` in built-in RBAC mode), if the kind binds its creator |

Each is optional: without it, nobody is bound, and the list is the full list. A child list without `X-Plat5-Member-Id` is unfiltered, so a front door serving a member must send it, as the gateway does on every kind route ([lists](../../concepts/access.md#lists)). When sent, `X-Plat5-Member-Id` must name an active member of the node's tree, and `X-Plat5-User-Id` an active user, or the request is **400**. Every other route ignores both.

## Nodes

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/nodes/{node_id}` | [Node](#node) |
| `PATCH` | `/nodes/{node_id}` | `{ display_name }` |
| `DELETE` | `/nodes/{node_id}` | Soft delete. Cascades to descendants, parents before children. Revokes nothing. **204**. Already deleted or purged: **204**, no change |
| `POST` | `/nodes/{node_id}/restore` | Within `deletion_grace`. Restores the node and the descendants its delete cascaded to (same `deletion_id`). Returns the node. A live node: **200**, no change. **409** `PARENT_DELETED` if the parent is deleted, **409** `GRACE_EXPIRED` if the grace has passed |
| `GET` | `/nodes/{node_id}/children/{kind}` | Nodes. [Filtered](../../concepts/access.md#lists) when `X-Plat5-Member-Id` is sent. `?status=deleted` for deleted ones. **422** if `{kind}` isn't a kind whose parent is this node's kind |
| `POST` | `/nodes/{node_id}/children/{kind}` | `{ display_name, slug? }`. **422** if `{kind}` isn't a kind whose parent is this node's kind. **409** `PARENT_DELETED` if this node is deleted. Returns the node, **201** |
| `POST` | `/roots/{kind}` | `{ display_name, slug? }`. A root kind. Returns the node, **201** |
| `GET` | `/nodes` | Every node, unfiltered: `?kind=`, `?parent_id=`, `?status=`. For other front doors |
| `POST` | `/nodes/{node_id}/release-slug` | <a id="slug-release"></a>On a purged root: frees its slug for a new root. **409** `SLUG_NOT_RELEASABLE` on a node that isn't a purged root. For an operator plane |
| `PATCH` | `/nodes/{root_id}/rate-limit-tier` | `{ rate_limit_tier }`: opaque tier name (`[a-z0-9:._-]+`, max 64) or `null` to clear. Root nodes only (**422** on a child node). For internal services and operator front doors; not generated in customer routes. Returns the node |

## Bindings

In external `authorizer` mode, binding writes (`POST`, `PATCH`, `DELETE`) are rejected with **422** (authorization lives in the external PDP).

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/nodes/{node_id}/bindings` | Every unexpired [binding](#binding) that counts here (member and group): on this node (`inherited: false`) and on each ancestor (`true`). `?inherited=false` for this node's own |
| `POST` | `/nodes/{node_id}/bindings` | `{ member_id, role, expires_at?, expires_in? }` or `{ group_id, role, expires_at?, expires_in? }` (exactly one of `member_id` or `group_id`; at most one of `expires_at` or `expires_in`). **422** if the member or group isn't in the node's tree, the role isn't bindable at the kind, or the expiry is invalid. Replaces an expired binding for the same `(subject, node, role)`; **409** if an unexpired one already exists. Returns the binding, **201** |
| `PATCH` | `/nodes/{node_id}/bindings/{binding_id}` | `{ role?, expires_at?, expires_in? }`, at least one required (`expires_at: null` clears expiry; `expires_at` and `expires_in` are mutually exclusive). **409** if changing `role` collides with an existing binding for that member or group here |
| `DELETE` | `/nodes/{node_id}/bindings/{binding_id}` | **204**, and again **204** once it's gone. A person's member left with no direct bindings and no group memberships is removed. A service account's isn't |
| `GET` | `/nodes/{node_id}/memberships/{member_id}` | [Membership](#membership): member id and status, plus unexpired bindings and permissions in built-in RBAC mode |
| `DELETE` | `/nodes/{root_id}/memberships/{member_id}` | The same as `DELETE /nodes/{root_id}/members/{member_id}` ([members](#members)), so a public `…/membership` route reads and leaves through one upstream. A root only; any other node is **404** |

## Groups

`/nodes/{root_id}/groups…` take a root; any other node is **404**. Groups are flat collections of active members (people or service accounts) within one tree. In external `authorizer` mode, group writes are rejected with **422**.

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/nodes/{root_id}/groups` | [Groups](#group) of the tree |
| `POST` | `/nodes/{root_id}/groups` | `{ name }`. Returns the group, **201** |
| `GET` | `/nodes/{root_id}/groups/{group_id}` | [Group](#group). **404** if not in this tree |
| `PATCH` | `/nodes/{root_id}/groups/{group_id}` | `{ name }`. Returns the group |
| `DELETE` | `/nodes/{root_id}/groups/{group_id}` | Delete the group, its bindings and its memberships. Final. **204**; already deleted: **204**, no change. A person's member left with no direct bindings and no group memberships is removed |
| `GET` | `/nodes/{root_id}/groups/{group_id}/members` | Active [members](#member) in the group |
| `POST` | `/nodes/{root_id}/groups/{group_id}/members` | `{ member_id }`. Must be an active member of this tree (**422** otherwise). **409** if already in the group. Returns the member, **201** |
| `DELETE` | `/nodes/{root_id}/groups/{group_id}/members/{member_id}` | Remove the member from the group. **204**; already absent: **204**, no change. A person's member left with no direct bindings and no group memberships is removed |

## Members

`/nodes/{root_id}/members…` take a root; any other node is **404**.

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/nodes/{root_id}/members` | Non-removed [members](#member) of the tree (active and suspended), guests marked |
| `POST` | `/nodes/{root_id}/members` | `{ idp: { issuer, subject }, role? }`. Resolves the person, creating them if the subject is new. In built-in RBAC mode, `role` is required and bound at the root; in external `authorizer` mode, `role` must be omitted (**422** if passed). **409** if already a member. Returns the member, **201** |
| `GET` | `/nodes/{root_id}/members/{member_id}` | [Member](#member). A removed member answers with `status: removed`. **404** if not in this tree |
| `PATCH` | `/nodes/{root_id}/members/{member_id}/status` | `{ status: active \| suspended }`. Reversibly suspends or reactivates a member within this tree without deleting bindings or rotating `member_id`. **422** on a removed member. Returns the member |
| `DELETE` | `/nodes/{root_id}/members/{member_id}` | Remove: bindings deleted, `member.removed`. Final. **204**; already removed: **204**, no change. A service account's member is **422**: delete the service account |
| `GET` | `/members/{member_id}/user` | The [user](#user) behind a person's member, removed members included. **404** for a service account's member or a deleted account |

## Users

People, by user id. Not behind any generated public route except the caller's own ([generated routes](../config/generated-routes.md)); for an operator plane and other front doors.

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/users?issuer=…&subject=…` | Exact lookup by IdP link, both required. `{ users: [...], has_more: false }`, with at most one |
| `GET` | `/users/{user_id}` | [User](#user) |
| `PATCH` | `/users/{user_id}` | `{ name }`: a string sets the person's name, `null` clears it ([a person's name](../idp.md#a-persons-name)). Returns the user |
| `DELETE` | `/users/{user_id}` | [Delete the account](../../concepts/lifecycle.md#deleting-an-account). **204**; already deleted: **204** |
| `PATCH` | `/users/{user_id}/status` | `{ status: active \| suspended }` |
| `GET` | `/users/{user_id}/nodes` | `?kind=` across every tree (omitted: every kind), [filtered](../../concepts/access.md#lists) to what the person can read. `?status=deleted` for deleted ones. Nodes |
| `GET`, `POST` | `/users/{user_id}/keys` | The person's personal [keys](#key). `POST { name?, allowed_cidrs?, scopes?, allowed_roots?, expires_at?, expires_in? }` returns the key once, **201** |
| `PATCH` | `/users/{user_id}/keys/{key_id}` | `{ name?, allowed_cidrs?, scopes?, allowed_roots?, expires_at?, expires_in? }`, at least one required. `[]` or `null` clears list fields; `expires_at: null` clears expiry. Returns the key |
| `DELETE` | `/users/{user_id}/keys/{key_id}` | Revoke. **204**, again **204** |
| `POST` | `/users/{user_id}/invites/redeem` | `{ token }`. **200** `{ member_id, node }`: the person's member in the invite's tree, and the [node](#node) it was for |

## Service accounts and keys

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/nodes/{node_id}/service-accounts` | [Service accounts](#service-account) that belong to this node |
| `POST` | `/nodes/{node_id}/service-accounts` | `{ name, role? }`. One transaction: principal, member in the node's tree, `member.created`, and (in built-in RBAC mode, where `role` is required) binding at the node. In external `authorizer` mode, `role` must be omitted (**422** if passed). Returns the service account, **201** |
| `GET`, `PATCH`, `DELETE` | `/nodes/{node_id}/service-accounts/{service_account_id}` | Addressed by its member id. **404** if it doesn't belong to this node. `PATCH { name }`. Delete, **204**: the principal and its keys are deleted and the member removed with `member.removed`; a lookup shows the removed member with no name. Already deleted: **204** |
| `GET`, `POST` | `/nodes/{node_id}/service-accounts/{service_account_id}/keys` | [Keys](#key), revoked ones included. `POST { name?, allowed_cidrs?, scopes?, expires_at?, expires_in? }` returns the key once, **201** |
| `PATCH` | `/nodes/{node_id}/service-accounts/{service_account_id}/keys/{key_id}` | `{ name?, allowed_cidrs?, scopes?, expires_at?, expires_in? }`, at least one required (`allowed_roots` is not accepted on service-account keys). `[]` or `null` clears list fields; `expires_at: null` clears expiry. Returns the key |
| `DELETE` | `/nodes/{node_id}/service-accounts/{service_account_id}/keys/{key_id}` | Revoke. **204**, again **204** |

A service account is a principal like a person: its bindings can sit anywhere in its own tree (`POST /nodes/{node_id}/bindings` with its member id), never outside it.

## Invites

| Method | Path | Notes |
|--------|------|--------|
| `GET`, `POST` | `/nodes/{node_id}/invites` | [Invites](#invite). Post: `{ role?, grants?, max_uses?, expires_in? }`, **201** with `token`, shown once. In built-in RBAC mode, `role` is required and `grants` is allowed on a root; in external `authorizer` mode, `role` and `grants` must be omitted (**422** if passed) |
| `DELETE` | `/nodes/{node_id}/invites/{invite_id}` | Revoke. **204**, again **204** |

`expires_in` is how long the token works, a duration (`7d`, `8h`: a whole number and `s`, `m`, `h` or `d`), default `7d`, from `60s` to `30d`. `max_uses` omitted is 1, a number is that many, and `null` is unlimited. `grants` are `[{ node_id, role }]`: live nodes below the root, each once, each role bindable at that node's kind.

A redeem binds what the person doesn't hold yet, for a new member and an existing one alike: `role` on the invite's node and, at a root, each grant on a node still live (in external `authorizer` mode, it creates the member if not yet in the tree and counts as a use when a new member is created). Someone not already in the tree becomes a member; redeemed below a root, a guest there. Where the member already has an unexpired binding, it stays as it is. A redeem that creates a member or binds anything is a use; one that changes nothing answers **200** and isn't a use. Unknown token: **404**. Redeemed, revoked or expired, an invite whose node is deleted, or a suspended user or member: **409** `INVITE_NOT_REDEEMABLE`, with `details.reason` (`redeemed`, `revoked`, `expired`, `node_deleted`, `user_suspended`, or `member_suspended`). A deleted node's invite works again if the node is restored before the invite expires.

Tokens are `{brand}-i-1-{secret}` ([secrets](#secrets)), hashed at rest and shown once, like keys. Lists carry `token_prefix` (the wire prefix plus 4 characters). A token that isn't of the deployment's brand and shape is unknown: **404**. When `use_count` reaches `max_uses` the invite is `redeemed`. The host sends any email; identity never mails.

## Roles

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/nodes/{node_id}/roles` | Roles bindable at this node's kind (predefined `system` roles and, when `custom_roles: true`, this root's `custom` roles), for role pickers. On a root, `?all=true` returns every role in the root regardless of `bindable_at` |
| `POST` | `/nodes/{root_id}/roles` | Create a custom role in this root (requires `custom_roles: true` in config; root only, **404** otherwise): `{ display_name, description?, permissions, bindable_at }`. Permissions must be in the assignable catalog (never `*`). `display_name` must be unique per root (case-insensitive) across custom roles and system role slugs (**409** `CONFLICT`). Returns the role, **201** |
| `GET` | `/nodes/{root_id}/roles/{role_id}` | Get a predefined role (by slug) or (when `custom_roles: true`) custom role (by id) in this root |
| `PATCH` | `/nodes/{root_id}/roles/{role_id}` | Update a custom role (`{ display_name?, description?, permissions?, bindable_at? }`; requires `custom_roles: true`). **422** on a predefined system role; **409** if `display_name` collides |
| `DELETE` | `/nodes/{root_id}/roles/{role_id}` | Delete a custom role (**204**; already deleted **204**). **422** on a predefined system role or in external `authorizer` mode. Removes any bindings referencing the role in this root (a person left with no bindings or groups is removed) |
| `GET` | `/nodes/{root_id}/permissions` | The deployment's permission catalog (`{ permissions: [{ name, description, bindable_at, custom_role_assignable }], custom_roles }`), for custom role editors. Root only |

```json
{
  "roles": [
    { "id": "admin", "slug": "admin", "display_name": "admin", "source": "system", "bindable_at": ["organization", "project"], "permissions": ["*"] },
    { "id": "01jby…", "slug": "01jby…", "display_name": "Billing Auditor", "description": "Read-only billing access", "source": "custom", "bindable_at": ["organization"], "permissions": ["billing:read"], "created_at": "2026-10-08T18:30:00Z", "updated_at": "2026-10-08T18:30:00Z" }
  ],
  "creator_role": "admin"
}
```

System roles appear first (sorted by slug), followed by custom roles (sorted by id). `permissions` are what each role grants, with `includes` resolved for system roles, sorted, with `*` as written. Not paginated.

## Security policy

`/nodes/{root_id}/security-policy` takes a root; any other node is **404**.

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/nodes/{root_id}/security-policy` | The root's [security policy](#security-policy-shape) (defaults when no row exists yet) |
| `PATCH` | `/nodes/{root_id}/security-policy` | `{ allow_personal_keys?, allowed_cidrs?, required_idp_claims? }`, at least one required. `allowed_cidrs` accepts up to 50 IPv4/IPv6 addresses or CIDRs (`[]` or `null` clears). `required_idp_claims` is a map of up to 16 claim paths (each must be listed in top-level `policy_claims` in config, **422** otherwise) to non-empty arrays of allowed string values (`{}` or `null` clears). Returns the updated policy |

## Shapes

Lists are [paginated](../lists.md) under the plural key. Times are RFC 3339, UTC. Every field holding an id ends in `_id`, except a resource's own `id`.

#### Node

`{ id, kind, parent_id, root_id, slug, display_name, status, rate_limit_tier, deleted_at, purge_after, created_at, updated_at, ancestors }`, on every response that returns a node. `status` is `active`, `deleted` or `purged`. `rate_limit_tier` is the opaque tier name on a root, or `null`. `purge_after` is when a deleted node is purged, `null` while live. `slug` is `null` once it's freed: on a purged node below a root, and on a root whose slug was [released](#slug-release), here and in `ancestors`. `ancestors` is `[{ id, kind, slug, display_name }]`, root first, without the node itself; `[]` on a root. A console builds its own URLs from it.

#### Binding

`{ id, member_id, group_id, node_id, role, inherited, guest, principal, name, expires_at, created_at, updated_at }`. Exactly one of `member_id` or `group_id` is set; the other is `null`. `node_id` is where the binding sits; `inherited` is true when that's an ancestor of the node listed. `expires_at` is RFC 3339 UTC or `null`. For a member binding, `guest`, `principal` (`person` or `service_account`) and `name` are the member's; for a group binding, `guest` is `false`, `principal` is `group`, and `name` is the group's name.

#### Security policy shape

<a id="security-policy-shape"></a>`{ allow_personal_keys, allowed_cidrs, required_idp_claims, updated_at }`. Defaults: `allow_personal_keys: true`, `allowed_cidrs: []`, `required_idp_claims: {}`, `updated_at: null`.

#### Group

`{ id, name, root_id, created_at, updated_at }`. Collection key `groups`.

#### Membership

`{ member_id, status, bindings?, permissions? }`: `status` is `active`, `suspended`, or `removed`. In built-in RBAC mode, also includes every unexpired [binding](#binding) that counts for that member at the node (both direct and through groups) and the `permissions` they grant (empty when the member is suspended or removed), sorted, with `*` as the role wrote it. In external `authorizer` mode, `bindings` and `permissions` are omitted.

#### Member

`{ id, principal, name, status, guest }`. `principal` is `person` or `service_account`; `status` is `active`, `suspended`, or `removed`; `guest` is true for a person's non-removed member with no unexpired effective binding at the root (neither directly nor through a group), and always false for a service account. `name` is null for a person who hasn't signed in, or whose account was deleted.

#### User

`{ id, name, status }`. `name` is `null` until it's known ([a person's name](../idp.md#a-persons-name)). `status` is `active` or `suspended`.

#### Service account

`{ id, name, node_id, created_at, updated_at }`. `id` is its member id; `node_id` is the node it belongs to. Collection key `service_accounts`.

#### Key

`{ id, name, key_prefix, allowed_cidrs, scopes, allowed_roots, expires_at, created_at, revoked_at }`. `allowed_cidrs` is the canonicalized IP/CIDR allowlist (`[]` when unrestricted); `scopes` is the sorted permission allowlist (`[]` when unrestricted); `allowed_roots` is the sorted root node ID allowlist (`[]` when unrestricted, personal keys only); `expires_at` is RFC 3339 UTC or `null`. On create, plus `key`, once.

#### Invite

`{ id, node_id, role, grants, max_uses, use_count, status, expires_at, token_prefix, created_at }`. `max_uses` is a number, or `null` for unlimited. `status` is `active`, `redeemed`, `revoked` or `expired`. On create, plus `token`, once.

## Field limits

| Field | Rule |
|-------|------|
| Node `display_name`, a person's `name`, group `name`, service account `name`, key `name` | 1 to 128 characters after trimming. A key's `name` defaults to `Unnamed key`; a person's is `null` until it's known |
| Key `allowed_cidrs` | Up to 50 IPv4/IPv6 addresses or CIDR blocks, canonicalized (bare IPs become `/32` or `/128`, host bits masked) and deduplicated. Empty or omitted: unrestricted |
| Invite `expires_in` | From `60s` to `30d`. Default `7d` |
| Invite `max_uses` | At least 1, or `null` for unlimited. Omitted → 1 |

## Errors

All responses use the [Plat5 error envelope](../errors.md).

| Case | Status |
|------|--------|
| A node, member, group, user, binding, key or invite not under the address given | **404** (service API). Through the gateway a missing node is already a **403** |
| Slug taken by a sibling, or by a root's tombstone; a person already a member; a member or group already holding an unexpired binding for that role on the node; a member already in a group; a custom role `display_name` colliding (case-insensitive) with another custom role or a system role slug | **409** `CONFLICT` (`{ field, value }`) |
| Slug malformed or reserved | **422** |
| Role not in the roles document or (when `custom_roles: true`) the root's custom roles, or not bindable at the kind | **422** |
| Direct binding for a member who isn't active in the node's tree, a group not in the node's tree, or sending neither or both of `member_id` and `group_id` | **422**: a member or group belongs to one tree, and a removed member is never active again |
| Built-in RBAC writes (bindings, groups, custom roles) or passing `role` / `grants` when external `authorizer` mode is active | **422** |
| `required_idp_claims` key not listed in control-plane `policy_claims` | **422** |
| Child kind, on a list or a create, that isn't a kind whose parent is this node's kind | **422** |
| `X-Plat5-Member-Id` naming no active member of the node's tree, or `X-Plat5-User-Id` naming no active user | **400** |
| An invite missing `role` in built-in RBAC mode, or below a root with `grants` | **422** |
| Removing a service account's member through the members route | **422**: a service account's member is never a person's; delete the service account |
| Restore with a deleted parent; create under a deleted node | **409** `PARENT_DELETED` (`{ parent_id }`) |
| Restore past `deletion_grace` | **409** `GRACE_EXPIRED` |
| Release the slug of a node that isn't a purged root | **409** `SLUG_NOT_RELEASABLE` |
| Any write to a purged node or to what hangs off it ([lifecycle](../../concepts/lifecycle.md#deleting-restoring-and-purging)): `PATCH`, a child create, adding or changing a binding, member or service account, creating, changing or revoking a service account's key or an invite. Not a slug release, and not a delete that finds nothing left (**204**) | **409** `NODE_PURGED` |
| Changing a revoked key (`PATCH`) | **409** `KEY_REVOKED` |
| Redeem of an invite that's redeemed, revoked or expired, or whose node is deleted | **409** `INVITE_NOT_REDEEMABLE` (`{ reason }`) |
| Postgres or external authorizer unavailable | **503** `SERVICE_UNAVAILABLE` |

## Audit details

Identity says what a write changed on `X-Plat5-Audit-Details` ([`concepts/audit.md`](../../concepts/audit.md#details)), as `{ "from", "to" }` for each changed field, and sets `X-Plat5-Audit-Created` on a create. Creates name what they made (`node_id`, `member_id`, `binding_id`, `key_id` with `key_prefix`, `invite_id`). Never a key, a token, a principal id or a user id.

A user route that changes a tree names it, so the event lands in that tree's logs too: `POST /roots/{kind}` names the new root and the creator's member; an invite redeem names the invite's node chain and the new or existing member. They use `X-Plat5-Audit-Nodes` and `X-Plat5-Audit-Member` ([`concepts/audit.md`](../../concepts/audit.md#named-nodes)).

## Credentials

Three credentials, all on the principal: an IdP token (people), a personal API key (people), a service account key. There are no sessions. Clients send each as `Authorization: Bearer`; the gateway sends anything that isn't a JWT to identity, so identity alone owns the key format ([secrets](#secrets)).

## Secrets

Identity mints two kinds of secret, both in one format:

```
{brand}-k-2-{secret}    API keys, personal and service account
{brand}-i-1-{secret}    invite tokens
```

| Part | |
|------|--|
| `brand` | `SECRET_BRAND`. `[a-z][a-z0-9]*`, max 32. Default `plat5`; empty-but-set refuses boot |
| `k`, `i` | What the secret is: a key or an invite token |
| `2`, `1` | The format's version for that kind of secret |
| `secret` | 32 random bytes, base64url without padding (43 characters) |

Both are hashed at rest (SHA-256 hex) and shown once. List responses carry `key_prefix` or `token_prefix` (the wire prefix plus 4 characters), never the secret. Changing the brand doesn't rewrite stored secrets: old keys stop authenticating and old invite tokens stop redeeming.

Storage: [`internals/identity-schema.md`](../../internals/identity-schema.md).
