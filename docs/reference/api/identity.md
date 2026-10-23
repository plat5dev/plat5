# Identity service API

What the [edge catalog](edge-catalog.md) points at, and what any other front door calls. Every path is ids; nothing here resolves slugs. Concepts: [access](../../concepts/access.md), [lifecycle](../../concepts/lifecycle.md).

The service is a function of the URL. Its own API addresses everything by id: `/nodes/{node_id}/…`, `/members/{member_id}/…`. Slugs exist only at the gateway edge; the generated catalog maps edge paths onto these.

Who may call is the front door. Identity doesn't know which front door called, and doesn't compare the caller to what it grants. The one place it reads the caller is `X-Plat5-Member-Id`: to make the creator's binding, to filter lists, and to act on the caller's own member. When sent, it must name an active member of the addressed node's tree, or the request is **400**; on `POST /roots/{kind}`, an active directory member.

## Nodes

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/nodes/{node_id}` | [Node](#node) |
| `PATCH` | `/nodes/{node_id}` | `{ label }` |
| `DELETE` | `/nodes/{node_id}` | Soft delete. Cascades to descendants, parents before children. Revokes keys of service accounts born at the node or below, and invites on the node or below. **204**. Already deleted or purged: **204**, no change. **422** on the sign-in node |
| `POST` | `/nodes/{node_id}/restore` | Within `deletion_grace`. Restores the node and the descendants that its delete cascaded to (same `deletion_id`). Keys and invites stay revoked. Returns the node. A live node: **200**, no change. **409** if the parent is deleted, the grace has passed, or the kind isn't in the config |
| `GET` | `/nodes/{node_id}/children/{kind}` | [Filtered](../../concepts/access.md#lists) by `X-Plat5-Member-Id`; **400** without it. `?state=deleted` for deleted ones. Nodes |
| `POST` | `/nodes/{node_id}/children/{kind}` | `{ label, slug? }`. `{kind}` must allow this node's kind as parent (**422**). **409** if this node is deleted or purged. The member in `X-Plat5-Member-Id`, if any, gets `creator_role`, unless an inherited binding with no `expires_at` already holds it. Returns the node, **201** |
| `POST` | `/roots/{kind}` | `{ label, slug? }`. A root kind other than the sign-in kind. `X-Plat5-Member-Id`, if any, is the caller's directory member; they get a member in the new tree with `creator_role` there. Returns the node, **201** |
| `GET` | `/nodes` | Every node, unfiltered: `?kind=`, `?parent_id=`, `?status=`. Not in the generated catalog; for other front doors |
| `POST` | `/nodes/{node_id}/release-slug` | <a id="slug-release"></a>On a purged node: frees its slug for a new node. Not in the generated catalog |

## Bindings and access

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/nodes/{node_id}/bindings` | [Bindings](#binding) on this node. One past its `expires_at` isn't listed |
| `POST` | `/nodes/{node_id}/bindings` | `{ member, role, expires_at? }`. **422** if the member isn't in the node's tree, the role isn't bindable at the kind, or `expires_at` breaks `max_duration`. **409** if the member already has a binding here. Returns the binding, **201** |
| `PATCH` | `/nodes/{node_id}/bindings/{binding_id}` | `{ role?, expires_at? }`. `expires_at: null` clears it. The result must still meet the role's `max_duration` |
| `DELETE` | `/nodes/{node_id}/bindings/{binding_id}` | **204**. A person's member left with no bindings is removed. A service account's isn't |
| `GET` | `/nodes/{node_id}/access` | [Access](#access) entries: every binding that counts here, direct and inherited |

## Members

`/nodes/{root_id}/members…` take a root other than the sign-in node; any other node is **404**. `/members/{member_id}…` take a directory member; any other member is **404**.

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/nodes/{root_id}/members` | Active [members](#member) of the tree, guests marked |
| `POST` | `/nodes/{root_id}/members` | `{ idp: { issuer, subject }, role?, expires_at? }`. Resolves the person, creating them and their directory member if the subject is new. Binds `role` or `default_role` at the root, expiring at `expires_at`; required when the role has a `max_duration`. **409** if already a member. Returns the member, **201** |
| `GET` | `/nodes/{root_id}/members/{member_id}` | [Member](#member). A removed member answers with `status: removed`. **404** if not in this tree |
| `DELETE` | `/nodes/{root_id}/members/{member_id}` | Remove: bindings deleted, `member.removed`. Final. **204**; already removed: **204**, no change. A service account's member is **422**: delete the service account |
| `GET` | `/members/{member_id}` | The person: `{ id, name, status }`, `status` being `active`, `suspended` or `removed` (a deleted account, with no name) |
| `DELETE` | `/members/{member_id}` | [Delete the account](../../concepts/lifecycle.md#deleting-an-account). **204** |
| `PATCH` | `/members/{member_id}/status` | `{ status: active \| suspended }`. Not in the generated catalog; for an operator plane |
| `GET` | `/members/{member_id}/nodes` | `?kind=` across every tree (omitted: every kind), [filtered](../../concepts/access.md#lists). `?state=deleted` for deleted ones. Nodes, each with `root_slug` and `root_label` |

## Service accounts and keys

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/nodes/{node_id}/service-accounts` | [Service accounts](#service-account) born at this node |
| `POST` | `/nodes/{node_id}/service-accounts` | `{ name, role, expires_at? }`. One transaction: principal, member in the node's tree, binding at the node, `member.created`. `role` required, and `expires_at` when the role has a `max_duration`. **422** on the sign-in node. Returns the service account, **201** |
| `GET`, `PATCH`, `DELETE` | `/nodes/{node_id}/service-accounts/{member_id}` | Addressed by its member id. **404** if not born at this node, or deleted. `PATCH { name }`. Delete, **204**: as at purge, the principal and its keys are deleted and the member removed with `member.removed`; a lookup shows the removed member with no name |
| `GET`, `POST` | `/nodes/{node_id}/service-accounts/{member_id}/keys` | [Keys](#key), revoked ones included. `POST { name? }` returns the key once, **201** |
| `DELETE` | `/nodes/{node_id}/service-accounts/{member_id}/keys/{key_id}` | Revoke. Idempotent. **204** |
| `GET`, `POST` | `/members/{member_id}/keys` | A directory member's personal keys, the same way |
| `DELETE` | `/members/{member_id}/keys/{key_id}` | Revoke. Idempotent. **204** |

A service account is a principal like a person: its bindings can sit anywhere in its own tree (`POST /nodes/{node_id}/bindings` with its member id), never outside it.

## Invites

| Method | Path | Notes |
|--------|------|--------|
| `GET`, `POST` | `/nodes/{node_id}/invites` | [Invites](#invite). Post: `{ role?, role_duration?, grants?, max_uses?, expires_at }`, **201** with `token`, shown once. `role` omitted → `default_role` on a root, **422** below one. `grants` only on a root. **422** on the sign-in node |
| `DELETE` | `/nodes/{node_id}/invites/{invite_id}` | Revoke. Idempotent. **204** |
| `POST` | `/members/{member_id}/invites/redeem` | `{ token }`. **200** `{ member_id, root_id, node_id }`: the member in the invite's tree, and the node it was for |

`expires_at` is when the token stops working. `role_duration` (and `duration` on each grant) is how long the binding it makes lasts, counted from redeem, as a duration (`7d`, `8h`: a whole number and `s`, `m`, `h` or `d`); required when the role has a `max_duration`, and no longer than it. `grants` are `[{ node, role, duration? }]`: ids of live nodes below the root, each once, each role bindable at that node's kind. The role is fixed when the invite is made: `default_role` then, if omitted.

Redeeming at a root makes a member with `role` at the root, plus each grant on a node still live. Redeeming below a root binds `role` at that node; someone not already in the tree becomes a guest there. Already holding a binding on that node: **200**, a use, no change, grants included. Unknown token: **404**. Redeemed, revoked or expired: **409** `CONFLICT` (`{ field: "status", value }`). An invite on a deleted node was revoked by the delete.

Token prefix `inv_`, hashed at rest and shown once, like keys. Lists carry `token_prefix` (`inv_` plus 4 characters). `max_uses` omitted → 1; `null` → unlimited. When `use_count` reaches `max_uses` the invite is `redeemed`. The host sends any email; identity never mails.

## Shapes

Lists are [paginated](../lists.md) under the plural key. Times are RFC 3339, UTC.

#### Node

`{ id, kind, parent_id, root_id, slug, label, status, deleted_at, created_at, updated_at }`. In `/members/{member_id}/nodes`, plus `root_slug` and `root_label`.

#### Binding

`{ id, member, node_id, role, expires_at, created_at, updated_at }`. `member` is the member id.

#### Access

`{ id, member, principal, name, role, node_id, inherited, expires_at, guest }`, one per binding that counts at the node: on it (`inherited: false`) or an ancestor (`true`), `node_id` being where it sits. `id` is the binding id. A binding past its `expires_at` isn't listed. On a root, guests' bindings below it are listed too, with `guest: true`. Collection key `access`.

#### Member

`{ id, principal, name, status, guest }`. `principal` is `person` or `service_account`; `status` is `active` or `removed`; `guest` is true for an active member with no binding at the root. `name` is null for a person who hasn't signed in, or whose account was deleted.

#### Service account

`{ id, name, node_id, created_at, updated_at }`. `id` is its member id; `node_id` is where it was born. Collection key `service_accounts`.

#### Key

`{ id, name, key_prefix, created_at, revoked_at }`. On create, plus `key`, once.

#### Invite

`{ id, node_id, role, role_duration, grants, max_uses, use_count, status, expires_at, token_prefix, created_at }`. `status` is `active`, `redeemed`, `revoked` or `expired`. On create, plus `token`, once.

## Roles

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/nodes/{node_id}/roles` | Roles bindable at this node's kind. Shape: [below](#roles) |

`GET /orgs/{organization}/roles`, and the same at each kind, returns the roles bindable there, for role pickers. At a project, with the roles above:

```
GET /orgs/{organization}/projects/{project}/roles
```

```json
{
  "roles": [
    { "slug": "admin", "labels": ["*"], "max_duration": null },
    { "slug": "developer", "labels": ["project:read", "catalog:write", "subscribers:write"], "max_duration": null },
    { "slug": "runtime", "labels": ["check"], "max_duration": null }
  ],
  "default_role": null,
  "creator_role": "admin"
}
```

Sorted by slug. Not paginated: the document is the whole list. `default_role` is `null` below a root. At the org, the same call lists `admin`, `developer`, `member`, `runtime` and `support`, with `default_role: "member"`.

## Field limits

| Field | Rule |
|-------|------|
| Node `label`, service account `name`, key `name` | 1 to 128 characters. A key's `name` defaults to `Unnamed key` |
| Invite `expires_at` | Between 60 seconds and 30 days from now |
| Invite `max_uses` | At least 1, or `null` for unlimited. Omitted → 1 |
| Binding `expires_at`, `role_duration`, grant `duration` | In the future, and within the role's `max_duration` |

Product sentences for each: [`reference/error-copy.md`](../error-copy.md#identity-specific-message).

## Errors

| Case | Status |
|------|--------|
| A node, member, binding, key or invite not under the address given | **404** (service API). Through the gateway a missing node is already a **403** |
| Slug taken by a sibling, live or tombstone | **409** `CONFLICT` |
| Slug malformed or reserved | **422** |
| Binding role not bindable at the kind, or longer than `max_duration` | **422** |
| Direct binding for someone who isn't in the tree | **422** |
| Child kind not allowed under this node's kind | **422** |
| A filtered list without `X-Plat5-Member-Id`, or that header naming no active member of the node's tree | **400** |
| An invite below a root without `role`, or with `grants` | **422** |
| Removing a service account's member through the members route | **422** |
| Service accounts or invites at the sign-in node | **422** |
| Restore with a deleted parent, past `deletion_grace`, or of a kind not in the config | **409** |
| Create under a deleted or purged node | **409** |
| Delete the sign-in node | **422** |

## Audit details

Identity says what a write changed on `X-Plat5-Audit-Details` ([`concepts/audit.md`](../../concepts/audit.md#details)), as `{ "from", "to" }` for each changed field, and sets `X-Plat5-Audit-Created` on a create. Creates name what they made (`node_id`, `member_id`, `binding_id`, `key_id` with `key_prefix`, `invite_id`). Never a key, a token, or a principal id.

A sign-in route that changes a tree names it, so the event lands in that tree's log too: `POST /roots/{kind}` names the new root and the creator's member; an invite redeem names the invite's root and the new or existing member. They use `X-Plat5-Audit-Root` and `X-Plat5-Audit-Member` ([`concepts/audit.md`](../../concepts/audit.md#root-and-member)).

## Credentials

Three credentials, all on the principal: an IdP token (people), a personal API key (people), a service account key. There are no sessions.

```
{brand}-k-2-{secret}
```

| Part | |
|------|--|
| `brand` | `APIKEY_BRAND`. `[a-z][a-z0-9]*`, max 32. Default `plat5`; empty-but-set refuses boot |
| `secret` | 32 random bytes, base64url without padding (43 characters) |

Keys are hashed at rest (SHA-256 hex) and shown once. List responses carry `key_prefix` (the wire prefix plus 4 characters), never the secret. Changing the brand doesn't rewrite stored secrets: old keys stop authenticating.

Storage and invariants: [`internals/identity-schema.md`](../../internals/identity-schema.md).
