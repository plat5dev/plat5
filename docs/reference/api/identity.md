# Identity service API

What identity's [resources](../config/resources.md) point at, and what any other front door calls. Every path is ids; nothing here resolves slugs. Concepts: [access](../../concepts/access.md), [lifecycle](../../concepts/lifecycle.md).

The service is a function of the URL. Its own API addresses everything by id: `/nodes/{node_id}/…`, `/users/{user_id}/…`. Slugs exist only at the gateway; identity's resources map public paths onto these.

Who may call is the front door. Identity doesn't know which front door called, and doesn't compare the caller to what it grants. It reads the caller on three routes only:

| Route | Reads | For |
|-------|-------|-----|
| `POST /nodes/{node_id}/children/{kind}` | `X-Plat5-Member-Id` | That member gets `creator_role` on the new node |
| `GET /nodes/{node_id}/children/{kind}` | `X-Plat5-Member-Id` | The list is filtered to what that member can read |
| `POST /roots/{kind}` | `X-Plat5-User-Id` | That user gets a member in the new tree, with `creator_role` on the root |

Each is optional: without it, nobody is bound and the list is the full list. When sent, `X-Plat5-Member-Id` must name an active member of the node's tree, and `X-Plat5-User-Id` an active user, or the request is **400**. Every other route ignores both.

## Nodes

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/nodes/{node_id}` | [Node](#node) |
| `PATCH` | `/nodes/{node_id}` | `{ display_name }` |
| `DELETE` | `/nodes/{node_id}` | Soft delete. Cascades to descendants, parents before children. Revokes nothing. **204**. Already deleted or purged: **204**, no change |
| `POST` | `/nodes/{node_id}/restore` | Within `deletion_grace`. Restores the node and the descendants its delete cascaded to (same `deletion_id`). Returns the node. A live node: **200**, no change. **409** if the parent is deleted or the grace has passed |
| `GET` | `/nodes/{node_id}/children/{kind}` | Nodes. [Filtered](../../concepts/access.md#lists) when `X-Plat5-Member-Id` is sent. `?status=deleted` for deleted ones |
| `POST` | `/nodes/{node_id}/children/{kind}` | `{ display_name, slug? }`. **422** if `{kind}`'s parent isn't this node's kind. **409** if this node is deleted or purged. Returns the node, **201** |
| `POST` | `/roots/{kind}` | `{ display_name, slug? }`. A root kind. Returns the node, **201** |
| `GET` | `/nodes` | Every node, unfiltered: `?kind=`, `?parent_id=`, `?status=`. For other front doors |
| `POST` | `/nodes/{node_id}/release-slug` | <a id="slug-release"></a>On a purged root: frees its slug for a new root. **409** on a node that isn't a purged root. For an operator plane |

## Bindings

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/nodes/{node_id}/bindings` | Every [binding](#binding) that counts here: on this node (`inherited: false`) and on each ancestor (`true`). `?inherited=false` for this node's own |
| `POST` | `/nodes/{node_id}/bindings` | `{ member_id, role }`. **422** if the member isn't in the node's tree or the role isn't bindable at the kind. **409** if the member already has a binding here. Returns the binding, **201** |
| `PATCH` | `/nodes/{node_id}/bindings/{binding_id}` | `{ role }` |
| `DELETE` | `/nodes/{node_id}/bindings/{binding_id}` | **204**, and again **204** once it's gone. A person's member left with no bindings is removed. A service account's isn't |
| `GET` | `/nodes/{node_id}/memberships/{member_id}` | [Membership](#membership): what that member holds here |

## Members

`/nodes/{root_id}/members…` take a root; any other node is **404**.

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/nodes/{root_id}/members` | Active [members](#member) of the tree, guests marked |
| `POST` | `/nodes/{root_id}/members` | `{ idp: { issuer, subject }, role }`, both required. Resolves the person, creating them if the subject is new. Binds `role` at the root. **409** if already a member. Returns the member, **201** |
| `GET` | `/nodes/{root_id}/members/{member_id}` | [Member](#member). A removed member answers with `status: removed`. **404** if not in this tree |
| `DELETE` | `/nodes/{root_id}/members/{member_id}` | Remove: bindings deleted, `member.removed`. Final. **204**; already removed: **204**, no change. A service account's member is **422**: delete the service account |
| `GET` | `/members/{member_id}/user` | The [user](#user) behind a person's member, removed members included. **404** for a service account's member or a deleted account |

## Users

People, by user id. Not behind any default public route except the caller's own ([resources](../config/resources.md)); for an operator plane and other front doors.

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/users?issuer=…&subject=…` | Exact lookup by IdP link, both required. `{ users: [...], has_more: false }`, with at most one |
| `GET` | `/users/{user_id}` | [User](#user) |
| `DELETE` | `/users/{user_id}` | [Delete the account](../../concepts/lifecycle.md#deleting-an-account). **204**; already deleted: **204** |
| `PATCH` | `/users/{user_id}/status` | `{ status: active \| suspended }` |
| `GET` | `/users/{user_id}/nodes` | `?kind=` across every tree (omitted: every kind), [filtered](../../concepts/access.md#lists) to what the person can read. `?status=deleted` for deleted ones. Nodes |
| `GET`, `POST` | `/users/{user_id}/keys` | The person's personal [keys](#key). `POST { name? }` returns the key once, **201** |
| `DELETE` | `/users/{user_id}/keys/{key_id}` | Revoke. **204**, again **204** |
| `POST` | `/users/{user_id}/invites/redeem` | `{ token }`. **200** `{ member_id, node }`: the person's member in the invite's tree, and the [node](#node) it was for |

## Service accounts and keys

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/nodes/{node_id}/service-accounts` | [Service accounts](#service-account) that belong to this node |
| `POST` | `/nodes/{node_id}/service-accounts` | `{ name, role }`, both required. One transaction: principal, member in the node's tree, binding at the node, `member.created`. Returns the service account, **201** |
| `GET`, `PATCH`, `DELETE` | `/nodes/{node_id}/service-accounts/{service_account_id}` | Addressed by its member id. **404** if it doesn't belong to this node. `PATCH { name }`. Delete, **204**: the principal and its keys are deleted and the member removed with `member.removed`; a lookup shows the removed member with no name. Already deleted: **204** |
| `GET`, `POST` | `/nodes/{node_id}/service-accounts/{service_account_id}/keys` | [Keys](#key), revoked ones included. `POST { name? }` returns the key once, **201** |
| `DELETE` | `/nodes/{node_id}/service-accounts/{service_account_id}/keys/{key_id}` | Revoke. **204**, again **204** |

A service account is a principal like a person: its bindings can sit anywhere in its own tree (`POST /nodes/{node_id}/bindings` with its member id), never outside it.

## Invites

| Method | Path | Notes |
|--------|------|--------|
| `GET`, `POST` | `/nodes/{node_id}/invites` | [Invites](#invite). Post: `{ role, grants?, max_uses?, expires_in? }`, **201** with `token`, shown once. `role` required. `grants` only on a root |
| `DELETE` | `/nodes/{node_id}/invites/{invite_id}` | Revoke. **204**, again **204** |

`expires_in` is how long the token works, a duration (`7d`, `8h`: a whole number and `s`, `m`, `h` or `d`), default `7d`, from `60s` to `30d`. `max_uses` omitted or `null` is 1; `unlimited` is unlimited. `grants` are `[{ node_id, role }]`: live nodes below the root, each once, each role bindable at that node's kind.

Redeeming at a root makes a member with `role` at the root, plus each grant on a node still live. Redeeming below a root binds `role` at that node; someone not already in the tree becomes a guest there. A redeem that changes nothing (already holding a binding on that node) answers **200** and isn't a use. Unknown token: **404**. Redeemed, revoked or expired: **409** `CONFLICT` (`{ field: "status", value }`). An invite whose node is deleted: **409** `CONFLICT` (`{ field: "node", value: "deleted" }`); it works again if the node is restored before the invite expires.

Token prefix `inv_`, hashed at rest and shown once, like keys. Lists carry `token_prefix` (`inv_` plus 4 characters). When `use_count` reaches `max_uses` the invite is `redeemed`. The host sends any email; identity never mails.

## Roles

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/nodes/{node_id}/roles` | Roles bindable at this node's kind, for role pickers |

```json
{
  "roles": [
    { "slug": "admin", "permissions": ["*"] },
    { "slug": "developer", "permissions": ["organization:read", "project:bindings:read", "project:create", "project:read", "project:write"] }
  ],
  "creator_role": "admin"
}
```

Sorted by slug. `permissions` are what the role grants, its `includes` resolved, sorted, with `*` as the role wrote it. Not paginated: the document is the whole list. With the [example roles](../config/roles.md), a project lists `admin` and `developer`, and an org lists `member` too.

## Shapes

Lists are [paginated](../lists.md) under the plural key. Times are RFC 3339, UTC. Every field holding an id ends in `_id`, except a resource's own `id`.

#### Node

`{ id, kind, parent_id, root_id, slug, display_name, status, deleted_at, purge_after, created_at, updated_at, ancestors }`, on every response that returns a node. `status` is `active`, `deleted` or `purged`. `purge_after` is when a deleted node is purged, `null` while live. `ancestors` is `[{ id, kind, slug, display_name }]`, root first, without the node itself; `[]` on a root. A console builds its own URLs from it.

#### Binding

`{ id, member_id, node_id, role, inherited, guest, principal, name, created_at, updated_at }`. `node_id` is where the binding sits; `inherited` is true when that's an ancestor of the node listed. `guest` is true when the member is a guest; `principal` and `name` are the member's.

#### Membership

`{ member_id, bindings, permissions }`: the bindings that count for that member at the node, each a [binding](#binding), and the permissions they grant there, sorted, with `*` as the role wrote it.

#### Member

`{ id, principal, name, status, guest }`. `principal` is `person` or `service_account`; `status` is `active` or `removed`; `guest` is true for a person's active member with no binding at the root, and always false for a service account. `name` is null for a person who hasn't signed in, or whose account was deleted.

#### User

`{ id, name, status }`. `status` is `active` or `suspended`.

#### Service account

`{ id, name, node_id, created_at, updated_at }`. `id` is its member id; `node_id` is the node it belongs to. Collection key `service_accounts`.

#### Key

`{ id, name, key_prefix, created_at, revoked_at }`. On create, plus `key`, once.

#### Invite

`{ id, node_id, role, grants, max_uses, use_count, status, expires_at, token_prefix, created_at }`. `max_uses` is a number or `"unlimited"`. `status` is `active`, `redeemed`, `revoked` or `expired`. On create, plus `token`, once.

## Field limits

| Field | Rule |
|-------|------|
| Node `display_name`, service account `name`, key `name` | 1 to 128 characters. A key's `name` defaults to `Unnamed key` |
| Invite `expires_in` | From `60s` to `30d`. Default `7d` |
| Invite `max_uses` | At least 1, or `unlimited`. Omitted or `null` → 1 |

Product sentences for each: [`reference/error-copy.md`](../error-copy.md#identity-specific-message).

## Errors

| Case | Status |
|------|--------|
| A node, member, user, binding, key or invite not under the address given | **404** (service API). Through the gateway a missing node is already a **403** |
| Slug taken by a sibling, or by a root's tombstone | **409** `CONFLICT` |
| Slug malformed or reserved | **422** |
| Role not in the roles document, or not bindable at the kind | **422** |
| Direct binding for someone who isn't in the tree | **422** |
| Child kind whose parent isn't this node's kind | **422** |
| `X-Plat5-Member-Id` naming no active member of the node's tree, or `X-Plat5-User-Id` naming no active user | **400** |
| An invite without `role`, or below a root with `grants` | **422** |
| Removing a service account's member through the members route | **422** |
| Restore with a deleted parent, or past `deletion_grace` | **409** |
| Create under a deleted or purged node | **409** |

## Audit details

Identity says what a write changed on `X-Plat5-Audit-Details` ([`concepts/audit.md`](../../concepts/audit.md#details)), as `{ "from", "to" }` for each changed field, and sets `X-Plat5-Audit-Created` on a create. Creates name what they made (`node_id`, `member_id`, `binding_id`, `key_id` with `key_prefix`, `invite_id`). Never a key, a token, a principal id or a user id.

A user route that changes a tree names it, so the event lands in that tree's logs too: `POST /roots/{kind}` names the new root and the creator's member; an invite redeem names the invite's node chain and the new or existing member. They use `X-Plat5-Audit-Nodes` and `X-Plat5-Audit-Member` ([`concepts/audit.md`](../../concepts/audit.md#named-nodes)).

## Credentials

Three credentials, all on the principal: an IdP token (people), a personal API key (people), a service account key. There are no sessions. Clients send each as `Authorization: Bearer`; the gateway sends anything that isn't a JWT to identity, so identity alone owns the key format:

```
{brand}-k-2-{secret}
```

| Part | |
|------|--|
| `brand` | `APIKEY_BRAND`. `[a-z][a-z0-9]*`, max 32. Default `plat5`; empty-but-set refuses boot |
| `secret` | 32 random bytes, base64url without padding (43 characters) |

Keys are hashed at rest (SHA-256 hex) and shown once. List responses carry `key_prefix` (the wire prefix plus 4 characters), never the secret. Changing the brand doesn't rewrite stored secrets: old keys stop authenticating.

Storage: [`internals/identity-schema.md`](../../internals/identity-schema.md).
