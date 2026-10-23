# Identity service API

What the [edge catalog](edge-catalog.md) points at, and what any other front door calls. Every path is ids; nothing here resolves slugs. Concepts: [access](../../concepts/access.md), [lifecycle](../../concepts/lifecycle.md).

The service is a function of the URL. Its own API addresses everything by id: `/nodes/{node_id}/…`, `/members/{member_id}/…`. Slugs exist only at the gateway edge; the generated catalog maps edge paths onto these.

Who may call is the front door. Identity doesn't know which front door called, and doesn't compare the caller to what it grants. The one place it reads the caller is `X-Plat5-Member-Id`: to make the creator's binding, to filter lists, and to act on the caller's own member.

## Nodes

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/nodes/{node_id}` | `{ id, kind, parent_id, root_id, slug, label, status, deleted_at, created_at, updated_at }` |
| `PATCH` | `/nodes/{node_id}` | `{ label }` |
| `DELETE` | `/nodes/{node_id}` | Soft delete. Cascades to descendants, parents before children. Revokes keys of service accounts born at the node or below |
| `POST` | `/nodes/{node_id}/restore` | Within `deletion_grace`. Restores the node and the descendants that its delete cascaded to (same `deletion_id`). Keys stay revoked. **409** if the parent is deleted |
| `GET` | `/nodes/{node_id}/children/{kind}` | [Filtered](../lists.md#filtered-lists) by `X-Plat5-Member-Id`; **400** without it. `?state=deleted` for deleted ones |
| `POST` | `/nodes/{node_id}/children/{kind}` | `{ label, slug? }`. `{kind}` must allow this node's kind as parent (**422**). The member in `X-Plat5-Member-Id`, if any, gets `creator_role`, unless an inherited binding already holds it |
| `POST` | `/roots/{kind}` | `{ label, slug? }`. A root kind other than the sign-in kind. `X-Plat5-Member-Id` is the caller's directory member; they get a member in the new tree with `creator_role` |
| `GET` | `/nodes` | Every node, unfiltered: `?kind=`, `?parent_id=`, `?status=`. Not in the generated catalog; for other front doors |
| `POST` | `/nodes/{node_id}/release-slug` | <a id="slug-release"></a>On a purged node: frees its slug for a new node. Not in the generated catalog |

## Bindings and access

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/nodes/{node_id}/bindings` | Bindings on this node |
| `POST` | `/nodes/{node_id}/bindings` | `{ member, role, expires_at? }`. **422** if the member isn't in the node's tree, the role isn't bindable at the kind, or `expires_at` breaks `max_duration`. **409** if the member already has a binding here |
| `PATCH` | `/nodes/{node_id}/bindings/{binding_id}` | `{ role?, expires_at? }` |
| `DELETE` | `/nodes/{node_id}/bindings/{binding_id}` | A person's member left with no bindings is removed. A service account's isn't |
| `GET` | `/nodes/{node_id}/access` | Every member with access here: direct bindings, and inherited ones with the node each comes from. Guests are marked `guest: true` |

## Members

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/nodes/{root_id}/members` | Active members of the tree, guests marked |
| `POST` | `/nodes/{root_id}/members` | `{ idp: { issuer, subject }, role?, expires_at? }`. Resolves the person, creating them if the subject is new. Binds `role` or `default_role` at the root, expiring at `expires_at`; required when the role has a `max_duration`. **409** if already a member |
| `GET` | `/nodes/{root_id}/members/{member_id}` | `{ id, principal, name, status }`. A removed member answers with `status: removed`. **404** if not in this tree |
| `DELETE` | `/nodes/{root_id}/members/{member_id}` | Remove: bindings deleted, `member.removed`. Final |
| `GET` | `/members/{member_id}` | A directory member: the person, `{ id, name, status }` |
| `DELETE` | `/members/{member_id}` | A directory member: [delete the account](../../concepts/lifecycle.md#deleting-an-account) |
| `PATCH` | `/members/{member_id}/status` | A directory member: `{ status: active \| suspended }`. Not in the generated catalog; for an operator plane |
| `GET` | `/members/{member_id}/nodes` | A directory member: `?kind=` across every tree, filtered as below. `?state=deleted` for deleted ones |

## Service accounts and keys

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/nodes/{node_id}/service-accounts` | Born at this node |
| `POST` | `/nodes/{node_id}/service-accounts` | `{ name, role }`. One transaction: principal, member in the node's tree, binding at the node, `member.created`. `role` required |
| `GET`, `PATCH`, `DELETE` | `/nodes/{node_id}/service-accounts/{member_id}` | Addressed by its member id. **404** if not born at this node. Delete removes the member and revokes its keys |
| `GET`, `POST` | `/nodes/{node_id}/service-accounts/{member_id}/keys` | `POST { name? }` returns the key once |
| `DELETE` | `/nodes/{node_id}/service-accounts/{member_id}/keys/{key_id}` | Revoke. Idempotent |
| `GET`, `POST` | `/members/{member_id}/keys` | A directory member's personal keys |
| `DELETE` | `/members/{member_id}/keys/{key_id}` | Revoke. Idempotent |

A service account is a principal like a person: its bindings can sit anywhere in its own tree (`POST /nodes/{node_id}/bindings` with its member id), never outside it.

## Invites

| Method | Path | Notes |
|--------|------|--------|
| `GET`, `POST` | `/nodes/{node_id}/invites` | Post: `{ role?, role_duration?, grants?, max_uses?, expires_at }`. Returns `token` while active. `grants` (`[{ node, role }]`, under the same root) only on a root |
| `DELETE` | `/nodes/{node_id}/invites/{invite_id}` | Revoke. Idempotent |
| `POST` | `/members/{member_id}/invites/redeem` | A directory member. `{ token }` |

`expires_at` is when the token stops working. `role_duration` (and `duration` on each grant) is how long the binding it makes lasts, counted from redeem; required when the role has a `max_duration`, and no longer than it. `grants` are `[{ node, role, duration? }]`.

Redeeming at a root makes a member with `role` (or `default_role`) at the root, plus each grant. Redeeming below a root binds `role` at that node; someone not already in the tree becomes a guest there. Already holding a binding on that node: **200**, a use, no change. Unknown token: **404**. Redeemed, revoked or expired: **409** `CONFLICT` (`{ field: "status", value }`).

Token prefix `inv_`, hashed at rest. `max_uses` omitted → 1; `null` → unlimited. When `use_count` reaches `max_uses` the invite is `redeemed`. The host sends any email; identity never mails.

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
| A filtered list without `X-Plat5-Member-Id` | **400** |
| Restore with a deleted parent, or past `deletion_grace` | **409** |

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
