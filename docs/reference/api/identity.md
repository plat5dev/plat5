# Identity service API

What identity's [generated routes](../config/generated-routes.md) point at, and what any other front door calls. Every path is ids; nothing here resolves slugs. Concepts: [access](../../concepts/access.md), [lifecycle](../../concepts/lifecycle.md).

The service is a function of the URL. Its own API addresses everything by id: `/nodes/{node_id}/…`, `/users/{user_id}/…`. Slugs exist only at the gateway; identity's service file maps public paths onto these.

Who may call is the front door. Identity doesn't know which front door called, and doesn't compare the caller to what it grants. It reads the caller on three routes only:

| Route | Reads | For |
|-------|-------|-----|
| `POST /nodes/{node_id}/children/{kind}` | `X-Plat5-Member-Id` | That member gets `creator_role` on the new node, if the kind binds its creator |
| `GET /nodes/{node_id}/children/{kind}` | `X-Plat5-Member-Id` | The list is filtered to what that member can read |
| `POST /roots/{kind}` | `X-Plat5-User-Id` | That user gets a member in the new tree, with `creator_role` on the root, if the kind binds its creator |

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
| `PATCH` | `/nodes/{root_id}/rate-limit-tier` | `{ rate_limit_tier }`: opaque tier name (`[a-z0-9:._-]+`, max 64) or `null` to clear. Root nodes only (**422** on a child node). For billing/operator front doors; not generated in customer routes. Returns the node |

## Bindings

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/nodes/{node_id}/bindings` | Every [binding](#binding) that counts here (member, group, and public): on this node (`inherited: false`) and on each ancestor (`true`). `?inherited=false` for this node's own |
| `POST` | `/nodes/{node_id}/bindings` | `{ member_id, role, expires_at?, expires_in? }`, `{ group_id, role, expires_at?, expires_in? }`, or `{ public_principal, role, expires_at?, expires_in? }` (exactly one of `member_id`, `group_id`, or `public_principal`; at most one of `expires_at` or `expires_in`). `public_principal` is `all_users` or `all_authenticated_users` and requires `allow_public: true` on the node's kind or an ancestor. **422** if the member or group isn't in the node's tree, `allow_public` isn't enabled, the role isn't bindable at the kind, or the expiry is invalid. **409** if that subject already holds that role here. Returns the binding, **201** |
| `PATCH` | `/nodes/{node_id}/bindings/{binding_id}` | `{ role?, expires_at?, expires_in? }`, at least one required (`expires_at: null` clears expiry; `expires_at` and `expires_in` are mutually exclusive). **409** if changing `role` collides with an existing binding for that member or group here |
| `DELETE` | `/nodes/{node_id}/bindings/{binding_id}` | **204**, and again **204** once it's gone. A person's member left with no direct bindings and no group memberships is removed. A service account's isn't |
| `GET` | `/nodes/{node_id}/memberships/{member_id}` | [Membership](#membership): every binding that counts for that member here (direct and through groups) |
| `DELETE` | `/nodes/{root_id}/memberships/{member_id}` | The same as `DELETE /nodes/{root_id}/members/{member_id}` ([members](#members)), so a public `…/membership` route reads and leaves through one upstream. A root only; any other node is **404** |

## Groups

`/nodes/{root_id}/groups…` take a root; any other node is **404**. Groups are flat collections of active members (people or service accounts) within one tree.

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
| `POST` | `/nodes/{root_id}/members` | `{ idp: { issuer, subject }, role }`, both required. Resolves the person, creating them if the subject is new. Binds `role` at the root. **409** if already a member. Returns the member, **201** |
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
| `POST` | `/nodes/{node_id}/service-accounts` | `{ name, role }`, both required. One transaction: principal, member in the node's tree, binding at the node, `member.created`. Returns the service account, **201** |
| `GET`, `PATCH`, `DELETE` | `/nodes/{node_id}/service-accounts/{service_account_id}` | Addressed by its member id. **404** if it doesn't belong to this node. `PATCH { name }`. Delete, **204**: the principal and its keys are deleted and the member removed with `member.removed`; a lookup shows the removed member with no name. Already deleted: **204** |
| `GET`, `POST` | `/nodes/{node_id}/service-accounts/{service_account_id}/keys` | [Keys](#key), revoked ones included. `POST { name?, allowed_cidrs?, scopes?, expires_at?, expires_in? }` returns the key once, **201** |
| `PATCH` | `/nodes/{node_id}/service-accounts/{service_account_id}/keys/{key_id}` | `{ name?, allowed_cidrs?, scopes?, expires_at?, expires_in? }`, at least one required (`allowed_roots` is not accepted on service-account keys). `[]` or `null` clears list fields; `expires_at: null` clears expiry. Returns the key |
| `DELETE` | `/nodes/{node_id}/service-accounts/{service_account_id}/keys/{key_id}` | Revoke. **204**, again **204** |

A service account is a principal like a person: its bindings can sit anywhere in its own tree (`POST /nodes/{node_id}/bindings` with its member id), never outside it.

## Invites

| Method | Path | Notes |
|--------|------|--------|
| `GET`, `POST` | `/nodes/{node_id}/invites` | [Invites](#invite). Post: `{ role, grants?, max_uses?, expires_in? }`, **201** with `token`, shown once. `role` required. `grants` only on a root |
| `DELETE` | `/nodes/{node_id}/invites/{invite_id}` | Revoke. **204**, again **204** |

`expires_in` is how long the token works, a duration (`7d`, `8h`: a whole number and `s`, `m`, `h` or `d`), default `7d`, from `60s` to `30d`. `max_uses` omitted is 1, a number is that many, and `null` is unlimited. `grants` are `[{ node_id, role }]`: live nodes below the root, each once, each role bindable at that node's kind.

A redeem binds what the person doesn't hold yet, for a new member and an existing one alike: `role` on the invite's node and, at a root, each grant on a node still live. Someone not already in the tree becomes a member; redeemed below a root, a guest there. Where the member already has a binding, it stays as it is. A redeem that binds anything is a use; one that binds nothing answers **200** and isn't a use. Unknown token: **404**. Redeemed, revoked or expired, or an invite whose node is deleted: **409** `INVITE_NOT_REDEEMABLE`, with `details.reason` (`redeemed`, `revoked`, `expired`, or `node_deleted`). A deleted node's invite works again if the node is restored before the invite expires.

Tokens are `{brand}-i-1-{secret}` ([secrets](#secrets)), hashed at rest and shown once, like keys. Lists carry `token_prefix` (the wire prefix plus 4 characters). A token that isn't of the deployment's brand and shape is unknown: **404**. When `use_count` reaches `max_uses` the invite is `redeemed`. The host sends any email; identity never mails.

## Roles

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/nodes/{node_id}/roles` | Roles bindable at this node's kind (predefined `system` roles and this root's `custom` roles), for role pickers. On a root, `?all=true` returns every role in the root regardless of `bindable_at` |
| `POST` | `/nodes/{root_id}/roles` | Create a custom role in this root (requires `custom_roles: true` in config; root only, **404** otherwise): `{ name, description?, permissions, bindable_at }`. Permissions must be in the assignable catalog (never `*`). **409** if `name` (case-insensitive) already exists in this root. Returns the role, **201** |
| `GET` | `/nodes/{root_id}/roles/{role_id}` | Get a predefined role (by slug) or custom role (by id) in this root |
| `PATCH` | `/nodes/{root_id}/roles/{role_id}` | Update a custom role (`{ name?, description?, permissions?, bindable_at? }`). **422** on a predefined system role |
| `DELETE` | `/nodes/{root_id}/roles/{role_id}` | Delete a custom role (**204**; already deleted **204**). **422** on a predefined system role. Existing bindings keep the role id and grant nothing |
| `GET` | `/nodes/{root_id}/permissions` | The deployment's permission catalog (`{ permissions: [{ name, description, bindable_at, custom_role_assignable }] }`), for custom role editors. Root only |

```json
{
  "roles": [
    { "id": "admin", "slug": "admin", "source": "system", "name": "admin", "description": null, "bindable_at": ["organization", "project"], "permissions": ["*"] },
    { "id": "01jby…", "slug": "01jby…", "source": "custom", "name": "Billing Auditor", "description": "Read-only billing access", "bindable_at": ["organization"], "permissions": ["billing:read"] }
  ],
  "creator_role": "admin"
}
```

System roles appear first (sorted by slug), followed by custom roles (sorted by name). `permissions` are what each role grants, with `includes` resolved for system roles, sorted, with `*` as written. Not paginated.

## Security policy

`/nodes/{root_id}/security-policy` takes a root; any other node is **404**.

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/nodes/{root_id}/security-policy` | The root's [security policy](#security-policy-shape) (defaults when no row exists yet) |
| `PATCH` | `/nodes/{root_id}/security-policy` | `{ allow_personal_keys?, allowed_cidrs?, required_idp_claims? }`, at least one required. `allowed_cidrs` accepts up to 50 IPv4/IPv6 addresses or CIDRs (`[]` or `null` clears). `required_idp_claims` is a map of up to 16 dotted claim paths to required string/bool/number values or non-empty arrays of allowed values (`{}` or `null` clears). Returns the updated policy |

## Shapes

Lists are [paginated](../lists.md) under the plural key. Times are RFC 3339, UTC. Every field holding an id ends in `_id`, except a resource's own `id`.

#### Node

`{ id, kind, parent_id, root_id, slug, display_name, status, rate_limit_tier, deleted_at, purge_after, created_at, updated_at, ancestors }`, on every response that returns a node. `status` is `active`, `deleted` or `purged`. `rate_limit_tier` is the opaque tier name on a root, or `null`. `purge_after` is when a deleted node is purged, `null` while live. `slug` is `null` once it's freed: on a purged node below a root, and on a root whose slug was [released](#slug-release), here and in `ancestors`. `ancestors` is `[{ id, kind, slug, display_name }]`, root first, without the node itself; `[]` on a root. A console builds its own URLs from it.

#### Binding

`{ id, member_id, group_id, public_principal, node_id, role, inherited, guest, principal, name, expires_at, created_at, updated_at }`. Exactly one of `member_id`, `group_id`, or `public_principal` (`all_users` or `all_authenticated_users`) is set; the others are `null`. `node_id` is where the binding sits; `inherited` is true when that's an ancestor of the node listed. `expires_at` is RFC 3339 UTC or `null`. For a member binding, `guest`, `principal` (`person` or `service_account`) and `name` are the member's; for a group binding, `guest` is `false`, `principal` is `group`, and `name` is the group's name; for a public binding, `guest` is `false`, `principal` is `public`, and `name` is the `public_principal` value.

#### Security policy shape

<a id="security-policy-shape"></a>`{ allow_personal_keys, allowed_cidrs, required_idp_claims, updated_at }`. Defaults: `allow_personal_keys: true`, `allowed_cidrs: []`, `required_idp_claims: {}`, `updated_at: null`.

#### Group

`{ id, name, root_id, created_at, updated_at }`. Collection key `groups`.

#### Membership

`{ member_id, bindings, permissions }`: every [binding](#binding) that counts for that member at the node (both direct and through groups), and the permissions granted by unexpired bindings there (empty when the member is suspended or removed), sorted, with `*` as the role wrote it.

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
| Slug taken by a sibling, or by a root's tombstone; a person already a member; a member, group or public principal already holding that role on the node; a member already in a group; a custom role name already taken in the root | **409** `CONFLICT` (`{ field, value }`) |
| Slug malformed or reserved | **422** |
| Role not in the roles document or the root's custom roles, or not bindable at the kind | **422** |
| Direct binding for a member who isn't active in the node's tree, a group not in the node's tree, a public binding where `allow_public` isn't enabled, or sending none or more than one of `member_id`, `group_id` and `public_principal` | **422**: a member or group belongs to one tree, and a removed member is never active again |
| Child kind, on a list or a create, that isn't a kind whose parent is this node's kind | **422** |
| `X-Plat5-Member-Id` naming no active member of the node's tree, or `X-Plat5-User-Id` naming no active user | **400** |
| An invite without `role`, or below a root with `grants` | **422** |
| Removing a service account's member through the members route | **422**: a service account's member is never a person's; delete the service account |
| Restore with a deleted parent; create under a deleted node | **409** `PARENT_DELETED` (`{ parent_id }`) |
| Restore past `deletion_grace` | **409** `GRACE_EXPIRED` |
| Release the slug of a node that isn't a purged root | **409** `SLUG_NOT_RELEASABLE` |
| Any write to a purged node or to what hangs off it ([lifecycle](../../concepts/lifecycle.md#deleting-restoring-and-purging)): `PATCH`, a child create, adding or changing a binding, member or service account, creating, changing or revoking a service account's key or an invite. Not a slug release, and not a delete that finds nothing left (**204**) | **409** `NODE_PURGED` |
| Changing a revoked key (`PATCH`) | **409** `KEY_REVOKED` |
| Redeem of an invite that's redeemed, revoked or expired, or whose node is deleted | **409** `INVITE_NOT_REDEEMABLE` (`{ reason }`) |
| Postgres or kind-level external authorizer unavailable | **503** `SERVICE_UNAVAILABLE` |

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
