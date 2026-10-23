# Identity service

Plat5 **identity**: the tree of nodes, the principals who can reach them (people and service accounts), their members and bindings, API keys, invites, admission for the gateway, and the lifecycle feed.

The model behind it: [`model.md`](model.md). Roles and bindings: [`roles.md`](roles.md). Errors: [`api-errors.md`](api-errors.md), [`error-copy.md`](error-copy.md). Lists: [`lists.md`](lists.md).

The service is a function of the URL. Its own API addresses everything by id: `/nodes/{node_id}/…`, `/members/{member_id}/…`. Slugs exist only at the gateway edge; the generated catalog maps edge paths onto these.

Who may call is the front door. Identity doesn't know which front door called, and doesn't compare the caller to what it grants. The one place it reads the caller is `X-Plat5-Member-Id`: to make the creator's binding, to filter lists, and to act on the caller's own member.

## Glossary

| Term | Meaning |
|------|---------|
| **node** | One instance of a kind from the `kinds` document. Wire id `n_…` |
| **root** | A node with no parent. Its tree is the isolation boundary |
| **sign-in node** | The single root of the `sign_in` kind (the directory), created at boot. No slug, no bindings |
| **principal** | A person or a service account. Its id never leaves identity |
| **member** | A principal in one tree. Wire id `m_…`. A person's member in the directory is their **user id** |
| **binding** | A role on a member at one node, optionally expiring. Wire id `b_…` |
| **guest** | A member with no binding on its root. Derived from bindings, never stored |
| **service account** | A non-human principal born at a node of an org's tree. Addressed by its member id in that tree |
| **api key** | A long-lived named credential on a principal: a person's personal key, or a service account's key |
| **invite** | A join token on a node. At a root it adds a member; below a root it can add a guest |

## Slugs

Every node except the sign-in node has a slug, fixed at creation. Fixed rules, not config:

| Rule | |
|------|--|
| Shape | Lowercase letters, digits and hyphens; starts with a letter, doesn't end with a hyphen; 3 to 63 characters. Never an id: ids contain `_` |
| Uniqueness | Per parent and kind. Roots share one namespace, so org slugs are deployment-wide |
| Reuse | Never, within the same parent. A purged node keeps its row as a tombstone (slug, id, kind, parent). A slug can be released by hand ([below](#slug-release)) |
| Generation | Omitted on create: generated from the label, with `-2`, `-3` on collision |
| Reserved | `reserved_slugs` in the `kinds` document. **422** |

A path segment that contains `_` is read as a node id, anything else as a slug. Both resolve under the previous node.

## Edge routes and the generated catalog

### Generated routes

Identity's edge routes are generated from the `kinds` document (`plat5 routes generate`, or `POST /catalog` on the control plane) and kept in the config like any routes file ([`control-plane.md`](control-plane.md#generated-catalog)). A new kind gets its API without code. Each kind gets a path segment (its `segment`, or its name plus `s`) and the labels below. Leaving a route out of the applied catalog, such as `POST /orgs`, is how a deployment restricts it.

### Generated labels

| Label | Checked at | Allows |
|-------|------------|--------|
| `<kind>:read` | The node | Getting the node; the node appearing in lists |
| `<kind>:create` | The parent | Creating one under the parent |
| `<kind>:write` | The node | Changing the label |
| `<kind>:delete` | The node | Deleting, listing deleted, restoring |
| `<kind>:bindings:read` / `:write` | The node | Listing or changing who has access here. On a root, also adding members and invites to join |
| `<kind>:service-accounts:write` | The node | Creating service accounts here and their keys |
| `<kind>:audit:read` | The node | Reading audit for this node and below |
| `<root kind>:guests:write` | The root | Invites on nodes below the root, which can admit people outside the tree. Granted to no role (counting `*`), guests are off |

**These are defaults, not rules.** They're the labels written into the generated catalog; the team can rename them, change them or drop routes. Identity's code depends on no label name ([filtered lists](#filtered-lists)). Roles grant them like any label; `*` grants all.

### Shown for `project` under `organization` (segment `orgs`)

| Edge method and path | Labels | Upstream on identity | Notes |
|---|---|---|---|
| `GET /orgs/{organization}/projects` | `[]` | `GET /nodes/{subject.organization_id}/children/project` | [Filtered](#filtered-lists). `?state=deleted` lists deleted ones the caller could delete |
| `POST /orgs/{organization}/projects` | `project:create` | `POST /nodes/{subject.organization_id}/children/project` | Body `{ label, slug? }`. The caller gets `creator_role`, unless an inherited binding already holds it |
| `GET /orgs/{organization}/projects/{project}` | `project:read` | `GET /nodes/{subject.project_id}` | |
| `PATCH …/{project}` | `project:write` | `PATCH /nodes/{subject.project_id}` | `{ label }`. The slug never changes |
| `DELETE …/{project}` | `project:delete` | `DELETE /nodes/{subject.project_id}` | Soft; cascades; revokes keys of service accounts born here and below |
| `POST …/{project}/restore` | `project:delete` | `POST /nodes/{subject.project_id}/restore` | `resolve_deleted: true` ([`routes.md`](routes.md#route-fields)). **409** if the parent is deleted |
| `GET, POST …/{project}/bindings` | `project:bindings:read`, `:write` | `/nodes/{subject.project_id}/bindings` | Body `{ member, role, expires_at? }`; `member` is a member id in the org |
| `PATCH, DELETE …/{project}/bindings/{binding_id}` | `project:bindings:write` | `/nodes/{subject.project_id}/bindings/{path.binding_id}` | Change role or expiry; remove |
| `GET …/{project}/access` | `project:bindings:read` | `GET /nodes/{subject.project_id}/access` | Direct and inherited bindings, each with the node it comes from |
| `GET, POST …/{project}/service-accounts` | `project:bindings:read`, `project:service-accounts:write` | `/nodes/{subject.project_id}/service-accounts` | `{ name, role }`: born here, bound here |
| `GET, PATCH, DELETE …/service-accounts/{service_account}` | `project:bindings:read`, `project:service-accounts:write` | `/nodes/{subject.project_id}/service-accounts/{path.service_account}` | `{ name }`. Delete removes its member and revokes its keys |
| `GET, POST …/service-accounts/{service_account}/keys` | `project:service-accounts:write` | `…/service-accounts/{path.service_account}/keys` | Plaintext once |
| `DELETE …/keys/{key_id}` | `project:service-accounts:write` | `…/keys/{path.key_id}` | Revoke |
| `GET, POST …/{project}/invites` | `organization:guests:write` | `/nodes/{subject.project_id}/invites` | `{ role, role_duration?, max_uses?, expires_at }`. Can admit a guest. To give someone already in the org a project role, bind them directly |
| `DELETE …/invites/{invite_id}` | `organization:guests:write` | `/nodes/{subject.project_id}/invites/{path.invite_id}` | Revoke |
| `GET …/{project}/roles` | `[]` | `GET /nodes/{subject.project_id}/roles` | Roles bindable here |

Roots get the same at `/orgs/{organization}`, with `organization:*` labels, except invites, which are the root's own (below). Plus:

| Edge method and path | Scope, labels | Upstream on identity | Notes |
|---|---|---|---|
| `POST /orgs` | `directory`, `[]` | `POST /roots/organization` | `{ label, slug? }`. Creates a root; the caller gets a member and `creator_role` |
| `GET, POST /orgs/{organization}/members` | `organization:bindings:read`, `:write` | `/nodes/{subject.organization_id}/members` | Post: `{ idp: { issuer, subject }, role?, expires_at? }`. `role` omitted → `default_role` |
| `GET /orgs/{organization}/members/{member}` | `[]` | `GET /nodes/{subject.organization_id}/members/{path.member}` | Name and status, a removed member too. So services and consoles can show "Dana (former member)" |
| `DELETE /orgs/{organization}/members/{member}` | `organization:bindings:write` | `DELETE /nodes/{subject.organization_id}/members/{path.member}` | Remove |
| `DELETE /orgs/{organization}/me` | `[]` | `DELETE /nodes/{subject.organization_id}/members/{subject.member_id}` | Leave the org |
| `GET, POST /orgs/{organization}/invites` | `organization:bindings:write` | `/nodes/{subject.organization_id}/invites` | `{ role?, role_duration?, grants?, max_uses?, expires_at }`. `grants` are bindings below the root made on redeem |
| `DELETE /orgs/{organization}/invites/{invite_id}` | `organization:bindings:write` | `/nodes/{subject.organization_id}/invites/{path.invite_id}` | Revoke |

The caller's own routes, all `scope: directory` with `[]`:

| Edge method and path | Upstream on identity | Notes |
|---|---|---|
| `GET /me` | `GET /members/{subject.member_id}` | The person: name, status |
| `DELETE /me` | `DELETE /members/{subject.member_id}` | Delete the account ([below](#account-deletion)) |
| `GET /me/nodes` | `GET /members/{subject.member_id}/nodes` | `?kind=project`: every node of that kind the person can read, across trees, with slug, label and the root's slug |
| `GET, POST /me/keys` | `/members/{subject.member_id}/keys` | Personal keys. They act as the person everywhere |
| `DELETE /me/keys/{key_id}` | `/members/{subject.member_id}/keys/{path.key_id}` | Revoke |
| `POST /me/invites/redeem` | `POST /members/{subject.member_id}/invites/redeem` | `{ token }` |

Service accounts have no directory member and can't call these.

The catalog also holds the audit service's read routes: `GET …/audit-events` at every kind with `<kind>:audit:read`, upstream `/nodes/{subject.<kind>_id}/audit-events`, and `GET /me/audit-events`, upstream `/members/{subject.member_id}/audit-events` ([`audit.md`](audit.md#reading)).

## Service API

What the catalog points at, and what any other front door calls. Every path is ids; nothing here resolves slugs.

### Nodes

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/nodes/{node_id}` | `{ id, kind, parent_id, root_id, slug, label, status, deleted_at, created_at, updated_at }` |
| `PATCH` | `/nodes/{node_id}` | `{ label }` |
| `DELETE` | `/nodes/{node_id}` | Soft delete. Cascades to descendants, parents before children. Revokes keys of service accounts born at the node or below |
| `POST` | `/nodes/{node_id}/restore` | Within `deletion_grace`. Restores the node and the descendants that its delete cascaded to (same `deletion_id`). Keys stay revoked. **409** if the parent is deleted |
| `GET` | `/nodes/{node_id}/children/{kind}` | [Filtered](#filtered-lists) by `X-Plat5-Member-Id`; **400** without it. `?state=deleted` for deleted ones |
| `POST` | `/nodes/{node_id}/children/{kind}` | `{ label, slug? }`. `{kind}` must allow this node's kind as parent (**422**). The member in `X-Plat5-Member-Id`, if any, gets `creator_role`, unless an inherited binding already holds it |
| `POST` | `/roots/{kind}` | `{ label, slug? }`. A root kind other than the sign-in kind. `X-Plat5-Member-Id` is the caller's directory member; they get a member in the new tree with `creator_role` |
| `GET` | `/nodes` | Every node, unfiltered: `?kind=`, `?parent_id=`, `?status=`. Not in the generated catalog; for other front doors |
| `POST` | `/nodes/{node_id}/release-slug` | <a id="slug-release"></a>On a purged node: frees its slug for a new node. Not in the generated catalog |

### Bindings and access

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/nodes/{node_id}/bindings` | Bindings on this node |
| `POST` | `/nodes/{node_id}/bindings` | `{ member, role, expires_at? }`. **422** if the member isn't in the node's tree, the role isn't bindable at the kind, or `expires_at` breaks `max_duration`. **409** if the member already has a binding here |
| `PATCH` | `/nodes/{node_id}/bindings/{binding_id}` | `{ role?, expires_at? }` |
| `DELETE` | `/nodes/{node_id}/bindings/{binding_id}` | A person's member left with no bindings is removed. A service account's isn't |
| `GET` | `/nodes/{node_id}/access` | Every member with access here: direct bindings, and inherited ones with the node each comes from. Guests are marked `guest: true` |

### Members

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/nodes/{root_id}/members` | Active members of the tree, guests marked |
| `POST` | `/nodes/{root_id}/members` | `{ idp: { issuer, subject }, role?, expires_at? }`. Resolves the person, creating them if the subject is new. Binds `role` or `default_role` at the root, expiring at `expires_at`; required when the role has a `max_duration`. **409** if already a member |
| `GET` | `/nodes/{root_id}/members/{member_id}` | `{ id, principal, name, status }`. A removed member answers with `status: removed`. **404** if not in this tree |
| `DELETE` | `/nodes/{root_id}/members/{member_id}` | Remove: bindings deleted, `member.removed`. Final |
| `GET` | `/members/{member_id}` | A directory member: the person, `{ id, name, status }` |
| `DELETE` | `/members/{member_id}` | A directory member: [delete the account](#account-deletion) |
| `PATCH` | `/members/{member_id}/status` | A directory member: `{ status: active \| suspended }`. Not in the generated catalog; for an operator plane |
| `GET` | `/members/{member_id}/nodes` | A directory member: `?kind=` across every tree, filtered as below. `?state=deleted` for deleted ones |

### Service accounts and keys

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

### Invites

| Method | Path | Notes |
|--------|------|--------|
| `GET`, `POST` | `/nodes/{node_id}/invites` | Post: `{ role?, role_duration?, grants?, max_uses?, expires_at }`. Returns `token` while active. `grants` (`[{ node, role }]`, under the same root) only on a root |
| `DELETE` | `/nodes/{node_id}/invites/{invite_id}` | Revoke. Idempotent |
| `POST` | `/members/{member_id}/invites/redeem` | A directory member. `{ token }` |

`expires_at` is when the token stops working. `role_duration` (and `duration` on each grant) is how long the binding it makes lasts, counted from redeem; required when the role has a `max_duration`, and no longer than it. `grants` are `[{ node, role, duration? }]`.

Redeeming at a root makes a member with `role` (or `default_role`) at the root, plus each grant. Redeeming below a root binds `role` at that node; someone not already in the tree becomes a guest there. Already holding a binding on that node: **200**, a use, no change. Unknown token: **404**. Redeemed, revoked or expired: **409** `CONFLICT` (`{ field: "status", value }`).

Token prefix `inv_`, hashed at rest. `max_uses` omitted → 1; `null` → unlimited. When `use_count` reaches `max_uses` the invite is `redeemed`. The host sends any email; identity never mails.

### Roles

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/nodes/{node_id}/roles` | Roles bindable at this node's kind. Shape: [`roles.md`](roles.md#listing-roles) |

### Filtered lists

`/nodes/{node_id}/children/{kind}` and `/members/{member_id}/nodes` return only the nodes the caller could read, as Google Cloud's project search returns only projects the caller has `resourcemanager.projects.get` on.

- **Which labels.** The labels of the kind's read route: the applied `GET` route to identity whose `upstream` is `/nodes/{subject.<kind>_id}`. Identity reads them from the services in its control-plane feed. With several such routes, any of their labels counts. With none applied, nothing is readable and the list is empty.
- **Where.** At each listed node, from the union of the caller's bindings on it and its ancestors ([`roles.md`](roles.md#resolution)).
- **Deleted.** `?state=deleted` uses the kind's delete route (`DELETE` to `/nodes/{subject.<kind>_id}`) the same way, counting the deleted node's own bindings too: they're kept until purge, so whoever could delete a node can find and restore it. That includes a deleted org, which has nothing above it. `GET /me/nodes?kind=organization&state=deleted` is how a person finds one.

This is why the generated list routes are `[]`: anyone in the tree may ask, and the answer is what they can read. A project-only member gets their projects from `GET /orgs/{organization}/projects`, not a 403.

### Account deletion

`DELETE /members/{member_id}` on a directory member, in one transaction: every member of the person in every tree is removed (bindings deleted, `member.removed` per tree), personal keys and IdP links are deleted, and the principal is deleted. Removed member rows stay, with no name. Nothing blocks it: an org left with no admin is recovered through an operator plane.

## Field limits

| Field | Rule |
|-------|------|
| Node `label`, service account `name`, key `name` | 1 to 128 characters. A key's `name` defaults to `Unnamed key` |
| Invite `expires_at` | Between 60 seconds and 30 days from now |
| Invite `max_uses` | At least 1, or `null` for unlimited. Omitted → 1 |
| Binding `expires_at`, `role_duration`, grant `duration` | In the future, and within the role's `max_duration` |

Product sentences for each: [`error-copy.md`](error-copy.md#identity-specific-message).

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

## Internal APIs

On **`INTERNAL_PORT`** only. Optional `INTERNAL_AUTH_TOKEN` → `X-Plat5-Internal-Token` (constant-time compare). Unset = network trust only (dev).

### Admit

One call that turns a credential and the route's node chain into the member and labels for this request. The gateway calls it on a cache miss ([`gateway-contract.md`](gateway-contract.md#cache)).

```
POST /internal/admit
{
  "credential": "e10s-k-2-…",          // or "idp": { "issuer", "subject", "name"? }
  "scope": "project",
  "chain": [ { "kind": "organization", "ref": "gruow" }, { "kind": "project", "ref": "gatheroom" } ],
  "resolve_deleted": false
}
→ 200
{
  "valid": true,
  "found": true,
  "principal": "person",
  "nodes": [ { "kind": "organization", "id": "n_…" }, { "kind": "project", "id": "n_…" } ],
  "member_id": "m_…",
  "labels": ["project:read", "catalog:write", "subscribers:write"],
  "good_until": "2026-11-01T00:00:00Z"
}
```

1. **Principal.** Hash the key, or look up the IdP link. A new IdP subject creates the person and their directory member; `name` (from the `name_claim`) sets or updates the person's name. A suspended person, a revoked key, or a service account whose born node is deleted is `valid: false`.
2. **Nodes.** Resolve each `ref` (slug or id) under the previous node, by kind. A missing, purged or deleted node ends here with `found: false`. With `resolve_deleted`, the last node may be deleted (not purged), and its own bindings still count. Nothing else ever counts a deleted node's bindings.
3. **Member.** The principal's active member in the chain's tree. None means `member_id: null` and `labels: []`.
4. **Labels.** The union of the roles of that member's live bindings on any node in the chain ([`roles.md`](roles.md#resolution)).
5. **Good until.** The earliest `expires_at` among the contributing bindings, or `null`.

On a sign-in-kind route, `chain` is empty, the member is the person's directory member, and `labels` is `[]`. A service account has none, so its keys are `valid: false` there.

| Result | Gateway |
|--------|---------|
| `valid: false` | **401** |
| `found: false`, or no route label in `labels` (and the route isn't `[]`), or `member_id: null` | **403** "permission denied, or it may not exist" |
| Otherwise | Forwarded with the subject |
| Transport error, non-2xx, missing fields | **503** |

### Plan counts

```
POST /internal/plan
{ "kinds_removed": ["folder"], "parents_narrowed": [{ "kind": "project", "removed": ["folder"] }] }
→ { "kinds_removed": { "folder": { "nodes": 14, "bindings": 22, "service_accounts": 3, "keys": 9 } }, "parents_narrowed": { "project": { "nodes": 3, "sample": ["n_…"] } } }
```

The control plane calls it on every dry run and again on every apply, to check the confirmed counts ([`control-plane.md`](control-plane.md#preconditions)). `nodes` counts the kind's nodes and their descendants, live and deleted, that the removal would delete.

### Lifecycle events

A feed of births and ends other services may act on. Written in the same transaction as the change; a cascade writes one event per node, parents before children. Reading is opt-in. Plat5 names no queue or broker; anyone who wants push delivery runs a relay that reads this feed.

```
GET /internal/lifecycle-events?starting_after=<id>&limit=<n>
{ "lifecycle_events": [
  { "id": "812", "type": "project.deleted", "node_id": "n_…", "root_id": "n_…", "occurred_at": "…" },
  { "id": "813", "type": "member.removed", "member_id": "m_…", "root_id": "n_…", "principal": "person", "occurred_at": "…" }
], "has_more": false }
```

| Event | When | A service should |
|-------|------|------------------|
| `<kind>.created` | A node is created | Allocate state if it wants to before the first request |
| `<kind>.deleted` | Soft delete, directly or by cascade | Stop background work; keep the data |
| `<kind>.restored` | Restore within the grace period | Resume |
| `<kind>.purged` | `deletion_grace` after delete | Delete the data |
| `member.created` | A principal joins a tree: first sign-in, a redeemed invite, an admin add, a service account create, a root create | Nothing usually |
| `member.removed` | Removed, left, account deleted, a person left with no bindings, a service account deleted, or its root purged | Remove records owned by that member that should go with it |

`principal` is `person` or `service_account`. No principal id, name or role. Binding changes aren't events: they change access, which is the gateway's concern, not service data.

`id` is a decimal string from a sequence; event writes are serialized, so `id` order is commit order. Pass it back as `starting_after`; don't do arithmetic on it. Delivery is at least once from the consumer's side, so acting on an event must be idempotent. Events are kept ([`../TODO.md`](../TODO.md)).

## Audit details

Identity says what a write changed on `X-Plat5-Audit-Details` ([`audit.md`](audit.md#details)), as `{ "from", "to" }` for each changed field, and sets `X-Plat5-Audit-Created` on a create. Creates name what they made (`node_id`, `member_id`, `binding_id`, `key_id` with `key_prefix`, `invite_id`). Never a key, a token, or a principal id.

A sign-in route that changes a tree names it, so the event lands in that tree's log too: `POST /roots/{kind}` names the new root and the creator's member; an invite redeem names the invite's root and the new or existing member. They use `X-Plat5-Audit-Root` and `X-Plat5-Audit-Member` ([`audit.md`](audit.md#root-and-member)).

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

## Data model

A fresh schema, `identity`.

```sql
CREATE TABLE nodes (
    id          TEXT PRIMARY KEY,
    kind        TEXT NOT NULL,
    parent_id   TEXT REFERENCES nodes (id),
    root_id     TEXT NOT NULL REFERENCES nodes (id),   -- itself for a root
    slug        TEXT,                                  -- NULL only for the sign-in node, or once released
    label       TEXT NOT NULL,
    status      TEXT NOT NULL CHECK (status IN ('active', 'deleted', 'purged')),
    deleted_at  TIMESTAMPTZ,
    deletion_id TEXT,                                  -- one per delete or kind removal; restore undoes exactly that one
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Slugs never reused: purged rows stay as tombstones and keep the index.
CREATE UNIQUE INDEX nodes_slug_uidx ON nodes (COALESCE(parent_id, ''), kind, slug) WHERE slug IS NOT NULL;

CREATE TABLE node_ancestors (                         -- closure table, depth 0 = itself
    node_id      TEXT NOT NULL REFERENCES nodes (id),
    ancestor_id  TEXT NOT NULL REFERENCES nodes (id),
    depth        INT  NOT NULL,
    PRIMARY KEY (node_id, ancestor_id)
);

CREATE TABLE principals (
    id            TEXT PRIMARY KEY,                    -- never sent to services
    kind          TEXT NOT NULL CHECK (kind IN ('person', 'service_account')),
    status        TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended')),
    name          TEXT,                                -- a service account's name; a person's from name_claim
    born_node_id  TEXT REFERENCES nodes (id),          -- service accounts only
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((kind = 'service_account') = (born_node_id IS NOT NULL))
);

CREATE TABLE idp_links (
    issuer TEXT NOT NULL, subject TEXT NOT NULL,
    principal_id TEXT NOT NULL REFERENCES principals (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (issuer, subject)
);

CREATE TABLE members (                                -- one per principal per tree: the id services see
    id            TEXT PRIMARY KEY,
    principal_id  TEXT REFERENCES principals (id) ON DELETE SET NULL,
    root_id       TEXT NOT NULL REFERENCES nodes (id),
    removed_at    TIMESTAMPTZ,                         -- set once; never cleared
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX members_active_uidx ON members (principal_id, root_id) WHERE removed_at IS NULL;

CREATE TABLE bindings (
    id          TEXT PRIMARY KEY,
    member_id   TEXT NOT NULL REFERENCES members (id),
    node_id     TEXT NOT NULL REFERENCES nodes (id),   -- under the member's root
    role        TEXT NOT NULL,                         -- grants nothing if the role is gone
    expires_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX bindings_uidx ON bindings (member_id, node_id);

CREATE TABLE api_keys (
    id            TEXT PRIMARY KEY,
    principal_id  TEXT NOT NULL REFERENCES principals (id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    key_prefix    TEXT NOT NULL,
    key_hash      TEXT NOT NULL UNIQUE,
    revoked_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE invites (
    id           TEXT PRIMARY KEY,
    node_id      TEXT NOT NULL REFERENCES nodes (id),  -- below a root: can admit a guest
    role         TEXT NOT NULL,
    grants       JSONB NOT NULL DEFAULT '[]',         -- [{node_id, role}] under the same root
    token_hash   TEXT NOT NULL UNIQUE,
    token_prefix TEXT NOT NULL,
    status       TEXT NOT NULL CHECK (status IN ('active', 'redeemed', 'revoked', 'expired')),
    max_uses     INT, use_count INT NOT NULL DEFAULT 0,
    expires_at   TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE lifecycle_events (                       -- append-only; outlives what it names
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    type        TEXT NOT NULL CHECK (type IN ('node.created', 'node.deleted', 'node.restored', 'node.purged',
                                              'member.created', 'member.removed')),
    kind        TEXT,                -- node events: the node's kind; on the wire: project.deleted
    node_id     TEXT,
    member_id   TEXT,
    root_id     TEXT NOT NULL,
    principal   TEXT CHECK (principal IN ('person', 'service_account')),
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

Ids are ULIDs with a type prefix: `n_` nodes, `m_` members, `b_` bindings, `k_` keys, `i_` invites. Principal ids never leave identity.

### Invariants the code enforces

- **Members.** One active member per principal per tree. In an org's tree it's created with its first binding; in the directory, at first sign-in. Removal sets `removed_at`, deletes its bindings and emits `member.removed`; it's never undone, and re-adding makes a new member. A person's member outside the directory left with no bindings, by delete or expiry, is removed. A service account's member is removed only with the service account (deleted directly, or with its born node); with no bindings its keys still authenticate and are refused everywhere, so a team can re-bind it.
- **Guests.** A member with no binding at the root. Not stored; read from the bindings.
- **Closed world.** A binding's node is under its member's root. A service account's member is in the tree of its `born_node_id`.
- **Directory.** One sign-in node, created at boot. Every person has one member there and no bindings; that member's id is their user id.
- **Roles.** A binding's `role` names a role in the roles document. A role that's missing, or not bindable at the binding's kind, grants nothing.
- **Expiry.** A binding past `expires_at` grants nothing; a sweeper deletes it.
- **Suspension.** A suspended principal's credentials are all invalid. There's no per-member suspension: remove the member instead.
- **Deletions.** Each delete, and each kind removal, stamps one `deletion_id` on every node it soft-deletes. A restore, or a kind applied again, restores exactly the nodes with that id. A node already deleted keeps its own id.
- **Deletion.** A node with `status = 'deleted'` grants nothing on it or below, except on a `resolve_deleted` route (restore, listing deleted). Once `deletion_grace` has passed, the purge sweeper sets `status = 'purged'`, deletes bindings on it and below, removes members of a purged root, and keeps the row.

## Runtime

| | |
|--|--|
| Directory | `services/identity/` |
| `service.name` / `service.namespace` | `identity` / `identity` |
| Public port | `3000` (the service API) |
| Internal port | `3001` (`/health/*`, `/metrics`, admit, plan counts, lifecycle events) |
| Database | Plat5 Postgres via `DATABASE_URL`, schema **`identity`** |
| `APIKEY_BRAND` | Default `plat5` ([credentials](#credentials)) |
| `CONTROL_PLANE_URL` | Required. Identity doesn't listen until it has `kinds` and `roles` ([`control-plane.md`](control-plane.md#internal-feeds)) |

Ready fails closed (**503**) when Postgres is unreachable or the documents aren't loaded.

## Non-goals

- Login UI, password store, JWKS (the IdP)
- A list or search of people for customers. `GET /nodes` and person status are for other front doors, not the catalog
- Principal ids on the wire, or as the way to add a member. Members are added by IdP subject or invite
- Sessions, or any credential bound to one tree
- Key labels or any other way to narrow a credential below its principal
- Service accounts bound outside their tree, or in the directory
- Caller checks: a grant cap, or comparing the caller to the member it acts on
- Role or label names in identity code
- Last-owner guards. An org without an admin is recovered by an operator plane
- Display-name overrides per tree. A person's name comes from the IdP
- SMTP (identity returns invite tokens; the console sends mail if it wants)
- A self-service API to link a second IdP subject (deferred, [`../TODO.md`](../TODO.md))
- Per-record permissions inside a node
