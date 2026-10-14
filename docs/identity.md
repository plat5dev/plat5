# Identity service

Plat5 **identity** service: the directory of people, organizations, members, invites, service accounts, credentials (API keys and sessions), and internal auth helpers for the gateway.

The model behind it: [`model.md`](model.md).

Boundary: [`identity-boundary.md`](identity-boundary.md). Errors: [`api-errors.md`](api-errors.md), [`error-copy.md`](error-copy.md). Lists: [`lists.md`](lists.md).

The service is a function of the URL. The path names every id the operation uses. If the handler does not read an id, it is not in the path.

Who may call is the proxy. This service does not know which proxy called, and it does not grow a second policy for a second caller. It does not know the caller's role or labels, and it does not compare the caller to what it grants.

Roles are the deployment's. Identity reads the roles document from the control plane, follows its changes without a restart, and resolves a member's role at validate. Model and rules: [`roles.md`](roles.md).

There is no `/api` prefix. The first path segment is the subject.

| Scope | Path | The call is about |
|-------|------|-------------------|
| user | `/users/{user_id}/...` | that person, in the directory |
| org | `/organizations/{organization_id}/...` | that org |
| member | `/members/{member_id}/...` | that member |

A member lives in one org. `member_id` is enough. Do not also put `organization_id` on a member route. Do not put a user id on an org route.

Public routes are not auto-published. The process still serves them on the public port. Internal validate stays on `INTERNAL_PORT`.

## Glossary

| Term | Meaning |
|------|---------|
| **realm** | A context that holds memberships: the **directory** or an **organization**. Credentials belong to a membership in one. |
| **directory** | The one realm every person belongs to. Private: customers cannot list or search people; staff can. No roles, admins, invites, or sessions; the IdP's JWT does the session's job. |
| **user** | A person. Lives in the directory. Wire id `user_id`, assigned by identity and linked to one or more IdP subjects (issuer + subject). |
| **organization** | The isolation boundary. A realm that users join and service accounts are born in. |
| **member** | A membership in an org. Exactly one of: a **user** or a **service account**. Wire id: `member_id`. |
| **membership** | An identity's standing in a realm. A person has one in the directory (their anchor: it holds their own keys, and has no status or role) and one per org they belong to (a member). On the wire, `/users/{user_id}/memberships` lists a person's org memberships. |
| **role** | Slug on a member. The deployment's roles document says which labels it grants. Plat5 defines no role names. Every member has one. [`roles.md`](roles.md) |
| **service account** | Non-human identity, born in an organization. Exactly one member, in that org. |
| **credential** | An API key or a session. Belongs to one membership and carries exactly its permissions: a member's credential carries the member's role labels. No labels of its own. |
| **api key** | Long-lived named credential. A **user key** belongs to the person's directory membership; a **member key** to a member. Same table, format, and validate. |
| **session** | Short-lived credential for one active user member in one org. No name, list, or revoke. Carries the member's role labels. |
| **display name** | A person's global name, set by the person. A member may override it for one org. A service account's display name is its name. |
| **invite** | Org join token (`active` / `redeemed` / `revoked` / `expired`). Redeem inserts an **active** member. The host sends any email. |

## What the service refuses

Illegal states. Not permissions.

- A member is exactly one of user or service account.
- One non-removed member row per `(organization_id, user_id)`. One member row per service account. A service account lives in exactly one org.
- A remove must leave at least one non-removed member. `active` and `suspended` both count. Service accounts count.
- Invite expiry, use limits, and the conflict on a dead token.
- A key addressed under a user, member, or service account that does not own it is **404**. That is the address, not an access check.
- A service account or member addressed under the wrong org, or whose member is `removed`, is **404**.
- Unknown id is **404**. A removed member is **404**, except a read at the org address, which returns what is left of it ([Former members](#former-members)). Empty collection is an empty page.
- A `role` that is not a slug in the roles document is **422**.
- A write may not take an org's non-removed members holding `creator_role` from one to zero (**422**).
- An account may not be deleted while its person is the last non-removed member, or the last `creator_role` holder, of an org (**409** `ORGANIZATION_HANDOFF_REQUIRED`).

Validation (**422**) and conflict (**409**) stay where the data is wrong. Identity does not decide who may call, and it does not compare the caller to what it grants. Whoever may call a route may assign any role and act on any member ([`roles.md`](roles.md#who-may-assign)).

## Public API

Pagination: [`lists.md`](lists.md). `limit`, `starting_after`, `has_more`, sort `id` ascending.

### Users

A person in the directory. First sign-in creates them: admission resolves the IdP issuer and subject to a `user_id`, creating the person when the subject is new ([validate](#credential-validate)). Adding a member by IdP subject does the same.

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/users/{user_id}` | The person. **404** if missing |
| `PATCH` | `/users/{user_id}` | Body `{ "display_name" }`. `null` clears it. A `status` field is **422**: status has its own route |
| `PATCH` | `/users/{user_id}/status` | Body `{ "status" }`: `active` or `suspended`. Not in the customer catalog; the operator gateway publishes it for staff |
| `DELETE` | `/users/{user_id}` | Delete the account. See below |
| `GET` | `/users` | Every person, as [user responses](#user-response). Body key `users`. Not in the customer catalog; the operator gateway publishes it for staff |

The directory is private to customers: no customer route lists or searches people. Staff list them through the operator gateway.

#### User response

```json
{
  "id": "...",
  "display_name": "Alice",
  "status": "active",
  "created_at": "...",
  "updated_at": "..."
}
```

`display_name` is `string | null`, max 128.

#### Person status

A person's `status` holds in every realm. `suspended` stops every credential the person has: their JWT, user keys, and every member key and session of theirs in any org. It is reversible. Only staff change it, through the operator gateway: the customer catalog leaves `PATCH /users/{user_id}/status` out, because a person who suspended themselves could not undo it.

#### Account deletion

`DELETE /users/{user_id}`, in one transaction:

- Every non-removed member of the person is removed, as a member remove does ([Status](#status)): keys revoked, sessions deleted, a `member.removed` lifecycle event per org.
- The person's user keys and IdP links are deleted, and the person is deleted.

Removed member rows stay, keeping their `member_id`. Their display name is gone ([Former members](#former-members)).

Refused with **409** `ORGANIZATION_HANDOFF_REQUIRED` while the person is the last non-removed member, or the last `creator_role` holder, of any org. `details.organizations` lists them as `{ id, name }`. The person hands those orgs to someone else, or deletes them, first.

### Memberships

Active **user** memberships only: not service accounts, not `suspended`, not `removed`.

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/users/{user_id}/memberships` | Which orgs this person belongs to |

#### Row

```json
{
  "id": "...",
  "organization": { "id": "...", "name": "Acme" },
  "role": "admin",
  "status": "active"
}
```

List body key `memberships`. `id` is the member id. Cursor is that `id`. `role` is `string | null`: `null` when roles are off ([`roles.md`](roles.md#the-model)).

A read of the person's org memberships, joined to their organizations.

### User API keys

The person's own keys. They belong to the person's directory membership. Member keys live under the member: `/members/{member_id}/api-keys`. Both are the same credential: one table, one [format](#credential-format), one [validate](#credential-validate).

| Method | Path | Notes |
|--------|------|--------|
| `POST` | `/users/{user_id}/api-keys` | Create; plaintext once. |
| `GET` | `/users/{user_id}/api-keys` | List (no hashes / no secret) |
| `DELETE` | `/users/{user_id}/api-keys/{key_id}` | Soft-revoke; idempotent. Other user's key → **404** |

#### Create body

```json
{ "name": "ci" }
```

`name` optional (default `Unnamed Key`, max 128).

A key carries its membership's permissions. A user key is the user: on `user` routes it is the same proof as a user JWT. A key has no labels of its own. The same holds for member keys and service-account keys.

Identity does not enforce route `required_labels`. That check is the gateway's, on routes the operator labeled. Keep redeem unlabeled when it is published — the invitee is not a member yet. A member's credential never passes on user routes, and a user's never on org routes. Gateway: [`gateway-contract.md`](gateway-contract.md), [`routes.md`](routes.md).

Never echo the secret except on create (`key`).

Create **201** also includes `"key": "{brand}-k-2-…"` once.

### Organizations

| Method | Path | Notes |
|--------|------|--------|
| `POST` | `/users/{user_id}/organizations` | Create. Body `{ "name" }`. Inserts an **active** member for that user with `creator_role`. Writes `organization.created`, then `member.created` for that member. |
| `GET` | `/organizations` | Every organization. |
| `GET` | `/organizations/{organization_id}` | **404** if missing |
| `PATCH` | `/organizations/{organization_id}` | Name. |
| `DELETE` | `/organizations/{organization_id}` | Hard delete. Cascades members, invites, service accounts, and keys. Missing → **404**. Not blocked by the last-member rule. |

#### Create body

```json
{ "name": "Acme" }
```

IDs are **ULID** strings. `name` max 128 chars.

#### Org response

```json
{
  "id": "...",
  "name": "Acme",
  "created_at": "...",
  "updated_at": "..."
}
```

### Members

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/organizations/{organization_id}/members` | Non-removed members. Unknown org → **404**. Empty org → empty page. |
| `POST` | `/organizations/{organization_id}/members` | Body `{ "idp": { "issuer", "subject" }, "role?" }`. Immediate `active`. `role` omitted → `default_role`. Writes `member.created`. |

A member has two addresses, like a service account. Both read and write the same row.

| Address | Who is acting | Published |
|---------|---------------|-----------|
| `/members/{member_id}` | The member, on itself | `member` scope, `/member` |
| `/organizations/{organization_id}/members/{member_id}` | The org, on one of its members | `organization` scope, `/org/members/{member_id}` |

Self address:

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/members/{member_id}` | **404** if missing or `removed` |
| `PATCH` | `/members/{member_id}` | Body `{ "status?", "display_name?" }`, at least one. `status`: `active` or `suspended` only; `removed` → **422**. `display_name`: this org's override; `null` clears it. Already `removed` → **404**. |
| `DELETE` | `/members/{member_id}` | Remove. Already `removed` → **404**. Last non-removed member → **422**. Last `creator_role` holder → **422**. |

The self address does not change `role`. A member may suspend or remove itself.

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/members/{member_id}/user` | `{ "user_id" }`: the person this member is. Removed members too. **404** for a service account, or once the person's account is deleted. Not in the customer catalog; the operator gateway publishes it, so staff can get from a member to the person |

Org address:

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/organizations/{organization_id}/members/{member_id}` | **404** if missing or in another org. `removed` → the [former member](#former-members) shape |
| `PATCH` | `/organizations/{organization_id}/members/{member_id}` | Body `{ "status?", "role?", "display_name?" }`, at least one. `status` and `display_name` as on the self address. **404** if missing, in another org, or `removed`. Last `creator_role` holder → **422**. |
| `DELETE` | `/organizations/{organization_id}/members/{member_id}` | Remove. Same **404** as patch. Last non-removed member → **422**. Last `creator_role` holder → **422**. |

Who may assign a role, and the last `creator_role` holder: [`roles.md`](roles.md).

`POST` adds a **user** member (immediately `active`). Identity resolves the person by the IdP issuer and subject, creating them in the directory if the subject is new. `user_id` stays internal: an admin never needs to know it. A non-removed duplicate is **409** `CONFLICT` (`field` is `idp`). A person who was removed gets a new member row with a new member id. The removed row stays removed.

Service accounts are created via the service-accounts API (member row included). Invites are a separate resource, below.

To replace the last person, add the new member first, then remove the old one. To destroy the org, `DELETE` the organization. That cascades, including the last member.

Suspending the last active member is allowed. The org still has a member.

#### Member response

```json
{
  "id": "...",
  "organization_id": "...",
  "principal": "user",
  "service_account_id": null,
  "display_name": "Alice",
  "role": "member",
  "status": "active",
  "created_at": "...",
  "updated_at": "..."
}
```

`principal` is `"user"` or `"service_account"`. `service_account_id` is set for a service account, else `null`. No `user_id`: an org sees its members, not the directory. `display_name` is the member's override for this org, else the person's global name, else `null`; for a service account, its name. `role` is `string | null`: `null` when roles are off ([`roles.md`](roles.md#the-model)).

#### Former members

A removed member's contributions can outlive it: comments, approvals, revisions. So a read by id at the org address still answers for a removed member, with only what a client needs to show its name:

```json
{ "id": "...", "organization_id": "...", "principal": "user", "display_name": "Alice", "status": "removed" }
```

`display_name` is `null` once the person's account is deleted. Lists never include removed members, and nothing else of the row is returned. A removed member is not writable at any address.

### Invites

Token invites. Membership is created only on redeem, as `active`. The host sends any email.

An invite is `active` while it can still be redeemed. Terminal statuses: `redeemed`, `revoked`, `expired`. Plaintext `token` is stored and returned only while `active`. `token_hash` is always stored (redeem lookup) and kept after the row is terminal.

List, redeem, and revoke expire lazily: if `expires_at` is in the past and status is still `active`, persist `expired` and null `token`.

| Method | Path | Notes |
|--------|------|--------|
| `POST` | `/organizations/{organization_id}/invites` | Body `{ "email?", "role?", "expires_in_seconds?", "max_uses?" }`. Returns `token`. Unknown org → **404**. `role` omitted → `default_role`. |
| `GET` | `/organizations/{organization_id}/invites` | `token` included while `active`. Unknown org → **404**. |
| `DELETE` | `/organizations/{organization_id}/invites/{invite_id}` | Revoke. Idempotent. Status `revoked`, `token` null. Hash stays. |
| `POST` | `/users/{user_id}/invites/redeem` | Body `{ "token" }`. Inserts an **active** member for that user and writes `member.created`. Already a member on a still-`active` token → **200** idempotent (counts as a use), and writes no event. A person who was removed gets a new member row. Unknown token → **404** `NOT_FOUND` (no org leak). Redeemed / revoked / expired → **409** `CONFLICT` (`field` is `status`, `value` is the terminal status). |

#### Create body

```json
{ "email": "a@b.com", "role": "member", "expires_in_seconds": 604800, "max_uses": 1 }
```

`role` is checked at create, not at redeem. `email` is optional display metadata; it is **not** mailed. `expires_in_seconds` default 7 days, min 60, max 30 days. `max_uses` omitted → 1. JSON `null` → unlimited. `0` and negatives → **422**.

Token prefix `inv_`. `token_hash` is SHA-256 hex. `use_count` increments on successful redeem. When `use_count` reaches `max_uses`, status becomes `redeemed` and `token` is nulled. Unlimited (`max_uses` null) stays `active` with `token`.

Redeem copies the invite's `role` onto the new member's `role`. A slug since removed from the roles document is copied as-is and grants nothing. Already a member: redeem does not change the existing role.

#### Create / list row

```json
{
  "id": "...",
  "organization_id": "...",
  "email": "a@b.com",
  "role": "member",
  "token_prefix": "inv_abcd",
  "token": "inv_…",
  "status": "active",
  "max_uses": 1,
  "use_count": 0,
  "expires_at": "...",
  "created_at": "..."
}
```

`token` is omitted once the row is terminal.

### Service accounts

Created under an organization. One transaction: service account row + **active** member, and `member.created`. Lives in **that org only**.

A service account is the org's. It is not tied to the member who created it: its role is its own, and its keys carry that role. Whoever may call the route may create one with any role ([`roles.md`](roles.md#who-may-assign)).

| Method | Path | Notes |
|--------|------|--------|
| `POST` | `/organizations/{organization_id}/service-accounts` | Body `{ "name", "role?" }`. Unknown org → **404**. `role` omitted → `service_account_default_role`, or `default_role` when that is unset. |
| `GET` | `/organizations/{organization_id}/service-accounts` | Non-removed. Unknown org → **404**. |
| `GET` | `/organizations/{organization_id}/service-accounts/{service_account_id}` | **404** if missing, wrong org, or member `removed` |
| `PATCH` | `/organizations/{organization_id}/service-accounts/{service_account_id}` | Body `{ "name" }`. Same **404** as get. |
| `DELETE` | `/organizations/{organization_id}/service-accounts/{service_account_id}` | Removes the member. Same **404** as get. Last non-removed member → **422**. Last `creator_role` holder → **422**. |

Lifecycle and role are the member row. Suspend, re-enable, or change role at the org address, `PATCH /organizations/{organization_id}/members/{member_id}`, with the service account's `member_id`. A removed service account is not addressable here, and its removal is final.

#### Service account response

```json
{
  "id": "...",
  "organization_id": "...",
  "member_id": "...",
  "name": "deploy-bot",
  "role": "member",
  "status": "active",
  "created_at": "...",
  "updated_at": "..."
}
```

`status` and `role` are the joined member's (`status` is `active` or `suspended`; `role` is `string | null`, `null` when roles are off). `removed` members are not listed and are not returned by id.

### Service account API keys

Org address for that service account's member keys. Not a separate credential: the same keys as `/members/{member_id}/api-keys`, the self address. A key created on either path is listed and revoked on both.

Who may call is the proxy, same as creating the service account. The key carries the service account's role, not the caller's.

| Method | Path | Notes |
|--------|------|--------|
| `POST` | `/organizations/{organization_id}/service-accounts/{service_account_id}/api-keys` | Create; plaintext once. Body same as member keys (`name`). |
| `GET` | `/organizations/{organization_id}/service-accounts/{service_account_id}/api-keys` | List. Collection key `keys`. Never the secret. |
| `DELETE` | `/organizations/{organization_id}/service-accounts/{service_account_id}/api-keys/{key_id}` | Soft-revoke. Idempotent. Key not under this service account → **404**. |

Missing service account, wrong org, or member `removed` → **404**, same as get. A `suspended` service account is addressable. Validate still rejects a key whose member is not `active`.

Published on `organization` scope as `/org/service-accounts/{service_account_id}/api-keys`. The client names the service account, not `member_id`.

### Member API keys

Keys that authenticate **as a member**. The parent path is the member, so the key belongs to that membership. When the member is a service account, the service-account path reads and writes these same rows.

| Method | Path | Notes |
|--------|------|--------|
| `POST` | `/members/{member_id}/api-keys` | Create; plaintext once. Body `{ "name" }`, as for user keys. |
| `GET` | `/members/{member_id}/api-keys` | List (never the secret) |
| `DELETE` | `/members/{member_id}/api-keys/{key_id}` | Soft-revoke. Idempotent. Other member's key → **404** |

Missing or `removed` member → **404**. A `suspended` member is addressable. Validate still rejects a key whose member is not `active`.

User keys and member keys are one credential: one table, one [format](#credential-format), one [validate](#credential-validate). The address a key is created under decides which membership it belongs to, and validate says which. Both use the `X-API-Key` header. Hashing at rest is SHA-256 hex. List may include revoked keys (`revoked_at` set).

### Member sessions

Short-lived credential for one **active user member** in one org. The same credential table and validate as keys, with kind `session` and its own [prefix](#credential-format). No name, no list, no revoke. Minting another session does not invalidate older ones. They expire.

TTL is **1 hour**. Not boot config.

Who may call is the proxy. A user JWT and a user API key are the same proof. Identity does not see which it was. The path `user_id` is the subject. A service account does not mint a session. It uses a member key.

| Method | Path | Notes |
|--------|------|--------|
| `POST` | `/users/{user_id}/organizations/{organization_id}/session` | No body. **201** returns the token once. Not an active member of that org (missing, `suspended`, `removed`, unknown org) → **404**. |

Empty `user_id` or `organization_id` → **422**. `user_id` longer than 128 → **422**.

The session carries the member's role labels. The role is not stored on the session: validate resolves it every time, so a role change reaches existing sessions.

#### Mint response

```json
{
  "token": "{brand}-s-2-…",
  "expires_at": "...",
  "member_id": "...",
  "organization_id": "...",
  "role": "admin",
  "labels": ["org:write", "org:members:write", "org:service-accounts:write"]
}
```

No `user_id`. `role` is `string | null` (`null` when roles are off). `labels` is a `string[]`: the member's role labels at mint, the same value validate would return now. A console can use it to show or hide actions. Hashing at rest is SHA-256 hex. Plaintext is returned once.

### Roles

The deployment's roles, from the roles document. Same list for every org today. Shape and rules: [`roles.md`](roles.md#listing-roles).

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/organizations/{organization_id}/roles` | `{ "roles": [{ "slug", "labels" }], "creator_role", "default_role", "service_account_default_role" }`. Sorted by slug. Not paginated. Unknown org → **404**. Roles off → `roles: []`, the three role fields `null`. |

### Credential format

```
{brand}-{kind}-{version}-{secret}
```

| Part | |
|------|--|
| `brand` | `APIKEY_BRAND`. `[a-z][a-z0-9]*`, max 32. Lowercase only — no folding |
| `kind` | `k` for an API key, `s` for a session |
| `version` | `2` |
| `secret` | 32 random bytes, base64url without padding (43 characters) |

The prefix says what a leaked string is and how to respond: a key is revoked; a session expires. It does not say whose it is. Validate returns the realm and the membership.

The brand is the deployment's, because the keys belong to its product: its customers hold them, and it registers the pattern with secret scanners. `APIKEY_BRAND` is identity's config alone; identity mints keys. Unset → `plat5`. Empty-but-set → refuse boot. One brand for the process, read at boot.

Version 1 credentials (`{brand}-sk-1-`, `{brand}-mk-1-`, `{brand}-ms-1-`) still validate.

Changing brand does not rewrite stored secrets. Old plaintext no longer matches; those rows cannot authenticate.

## Audit details

Identity says what a write changed on `X-Plat5-Audit-Details` ([`audit.md`](audit.md#details)). The gateway puts it on the audit event when the route is audited, and strips it either way. Identity does not know who called or whether the route is audited. It only reports the change.

Changes are `{ "from", "to" }`, for the fields that changed. A write that changed nothing sends no header. A create names what it created, since the new id is not in the path, and sets `X-Plat5-Audit-Created` to the new id ([`audit.md`](audit.md#created)).

A `user` route that changes something an org owns names that org and member on `X-Plat5-Audit-Organization` and `X-Plat5-Audit-Member`, so the event is in the org's log too ([`audit.md`](audit.md#which-log)):

| Write | Org and member named |
|-------|----------------------|
| `POST /users/{user_id}/organizations` | The new org, and its creator member |
| `POST /users/{user_id}/invites/redeem` | The org, and the new (or existing) member |
| `POST /users/{user_id}/organizations/{organization_id}/session` | The org, and the member the session is for |

| Write | Details |
|-------|---------|
| `PATCH /organizations/{organization_id}` | `name` as a change |
| `POST /organizations/{organization_id}/members` | `member_id`, `role` |
| `PATCH /organizations/{organization_id}/members/{member_id}` | `role`, `status`, `display_name` as changes |
| `PATCH /members/{member_id}` | `status`, `display_name` as changes |
| `PATCH /users/{user_id}` | `display_name` as a change |
| `PATCH /users/{user_id}/status` | `status` as a change |
| `POST /users/{user_id}/api-keys` | `key_id`, `key_prefix`, `name`. Never the key |
| `POST /organizations/{organization_id}/invites` | `invite_id`, `role`. Never the token |
| `POST /organizations/{organization_id}/service-accounts` | `service_account_id`, `member_id`, `name`, `role` |
| `PATCH /organizations/{organization_id}/service-accounts/{service_account_id}` | `name` as a change |
| `POST /members/{member_id}/api-keys`, `POST /organizations/{organization_id}/service-accounts/{service_account_id}/api-keys` | `key_id`, `key_prefix`, `name`. Never the key. `key_prefix` is what later events show as the actor's |

```json
{ "role": { "from": "developer", "to": "admin" } }
```

Deletes, revokes, and removals send none: the route and its path params say what happened. `role` is `null` while roles are off. No `user_id`, email, key, or token.

Each old value is read under the lock the write already takes. Reporting it costs no extra query.

## Status

Status is whether the member is admitted, not what they are allowed to do. A person's own [status](#person-status) stops all of their members at once.

| Status | |
|--------|--|
| `active` | Admitted |
| `suspended` | Not admitted |
| `removed` | Final. Not listed, not addressable, never made `active` again |

`PATCH` accepts `active` or `suspended` only. `removed` is `DELETE`.

`suspended` is the reversible state. `removed` is not. A remove, at either address or by the service-account delete, does all of this in one transaction:

- Status → `removed`.
- Every member key is revoked (`revoked_at` set). Every member session is deleted.
- A `member.removed` lifecycle event is written ([Lifecycle events](#lifecycle-events)).

The row stays as a record that the member existed. Adding the same person again, by `POST` or invite redeem, inserts a new member with a new `member_id`. Nothing of the removed member carries over: no keys, no sessions, no role.

## Internal APIs

Not published on the gateway. Served only on **`INTERNAL_PORT`**. Optional `INTERNAL_AUTH_TOKEN` → header `X-Plat5-Internal-Token` (constant-time compare). Unset = network-trust only (dev). Validate is a lookup for a proxy, and lifecycle events are a feed for services. Neither is a check on the resource handlers. Do not fold them into the resource URLs.

Gateway env: `CREDENTIAL_VALIDATE_URL`, same `INTERNAL_AUTH_TOKEN`. Required to boot. `APIKEY_CACHE_TTL_SECS` covers every credential and every IdP subject resolve.

### Credential validate

One endpoint for every credential. The gateway sends either the `X-API-Key` value, or a verified JWT's issuer and subject.

```
POST /internal/credentials/validate
Content-Type: application/json
X-Plat5-Internal-Token: <INTERNAL_AUTH_TOKEN>   # when token is set

{ "credential": "{brand}-k-2-…" }
{ "idp": { "issuer": "https://auth.example.com", "subject": "…" } }
```

| Result | Response |
|--------|----------|
| A user key, or an IdP subject | **200** `{ "valid": true, "realm": "directory", "kind": "key" \| "idp", "user_id": "…" }` |
| A member key or session | **200** `{ "valid": true, "realm": "organization", "kind": "key" \| "session", "member_id": "…", "organization_id": "…", "labels": ["*"] }` or `"labels": ["org:write"]` or `"labels": []` |
| Malformed, missing, revoked, expired, unknown; member not `active`; person `suspended` | **200** `{ "valid": false }` |

An IdP subject resolves to its person, and a new subject creates one, with its directory membership. The person's `status` applies to every result: a suspended person's JWT, user keys, and member credentials are all `valid: false`.

`labels` is a `string[]`, never `null`: the member's role labels ([`roles.md`](roles.md#resolution)). `["*"]` = every label (a `["*"]` role, or roles off). `[]` = no labels (**403** on routes with `required_labels`; unlabeled still admit). Role resolution happens here. The response carries no role. A directory result has no `labels`: a person has no role.

An organization result has no `user_id`.

Gateway: [`gateway-contract.md`](gateway-contract.md#credentials). It caches valid and invalid results (`APIKEY_CACHE_TTL_SECS`, default 300s). Revoke, suspend, and role changes are visible at the edge when the TTL expires. Transport / 503 are not cached.

| Validate outcome | Client |
|------------------|--------|
| `valid: false`, or the wrong realm for the route's scope | **401** `UNAUTHORIZED` |
| transport / non-2xx / `valid: true` missing required fields | **503** `SERVICE_UNAVAILABLE` |

### Lifecycle events

A feed of facts other services may act on: the birth or the end of an organization or a member. A service that holds data keyed by those ids cleans up on an end, and may allocate state on a birth before any request arrives. Every birth and end is recorded. Reading is opt-in: a service that does not read the feed is not affected by it. Plat5 names no queue or broker. Anyone who wants push delivery runs a relay that reads this feed.

```
GET /internal/lifecycle-events?starting_after=<id>&limit=<n>
X-Plat5-Internal-Token: <INTERNAL_AUTH_TOKEN>   # when token is set
```

```json
{
  "lifecycle_events": [
    { "id": "1041", "type": "organization.created", "occurred_at": "…", "organization_id": "…" },
    { "id": "1042", "type": "member.created", "occurred_at": "…", "organization_id": "…", "member_id": "…", "principal": "user" },
    { "id": "1043", "type": "organization.deleted", "occurred_at": "…", "organization_id": "…" },
    { "id": "1044", "type": "member.removed", "occurred_at": "…", "organization_id": "…", "member_id": "…", "principal": "service_account" }
  ],
  "has_more": false
}
```

| Type | Written by | Fields |
|------|------------|--------|
| `organization.created` | `POST /users/{user_id}/organizations` | `organization_id` |
| `organization.deleted` | `DELETE /organizations/{organization_id}` | `organization_id` |
| `member.created` | the founder on org create, `POST /organizations/{organization_id}/members`, invite redeem when it inserts a member, and service-account create | `organization_id`, `member_id`, `principal` |
| `member.removed` | `DELETE` at either member address, the service-account delete, and account deletion (one per org) | `organization_id`, `member_id`, `principal` |

`principal` is `user` or `service_account`. No `user_id`, name, role, or anything else from the row. A service account is a member: its create is `member.created`, not its own type.

`member.removed` is final. A person added again is a new `member_id` and a new `member.created`. A service never undoes a remove.

An org create writes `organization.created`, then `member.created` for the founder, in that order, in the same transaction. An org delete writes one `organization.deleted`, not a `member.removed` per member. It covers everything in the org.

Not events: suspend (of a member or a person), role changes, org rename, display names, keys, sessions, and invites. An invite redeem that finds an existing member writes nothing. The directory has no events: an account deletion is the `member.removed` events it causes, and creating a person writes none. A suspend or role change is not an end, and anything that depends on it is decided when the work runs. Services hold nothing keyed by keys, sessions, or invites. The feed is not a copy of the row: a consumer that needs a name or a role reads it.

**Delivery.** The event is written in the same transaction as the change. A change that commits has its event. A change that rolls back has none. The org delete is a hard cascade, so after it commits the event is the only record that the org existed.

**Order.** Pagination is as in [`lists.md`](lists.md), `id` ascending, with one exception: `id` is not a ULID. It is a decimal string from a sequence, and `starting_after` takes that form. Pass it back as given; do not do arithmetic on it. Writes of events are serialized (a transaction-level advisory lock taken before the insert), so `id` order is commit order. A reader that has seen an `id` will never later see a smaller one.

**Reading.** A consumer keeps the last `id` it processed and passes it as `starting_after`. Omit it to read from the first event. Delivery is at least once from the consumer's side: if it crashes after acting but before saving the `id`, it reads the event again. Acting on an event must be idempotent.

Events are kept. There is no retention yet ([`../TODO.md`](../TODO.md)).

## Data model (logical)

```
identities             -- persons and service accounts
  id, kind (user | service_account), status, …
  service_account: organization_id, name
idp_links              -- a person's IdP subjects
  identity_id, issuer, subject
  unique (issuer, subject)
organizations
memberships            -- an identity's standing in a realm
  identity_id, organization_id?          -- null = the directory
  display_name?                          -- directory: global name; org: override
  role?, status                          -- org memberships only
  unique (identity_id) where organization_id is null
  unique (organization_id, identity_id) where status <> 'removed'
credentials            -- every API key and session
  membership_id, kind (key | session), name?, key_prefix, key_hash, expires_at?, revoked_at?, …
organization_invites   -- token while active; token_hash always
  organization_id, email?, role?, token?, token_hash, status, max_uses, use_count, expires_at, …

lifecycle_events       -- append-only; no FK, outlives the org it names
  id (bigint sequence), type, occurred_at, organization_id, member_id?, principal?
```

Wire ids: `user_id` is a person's identity id. `service_account_id` is a service account's. `member_id` is an org membership's. A person's directory membership has no wire id: `/users/{user_id}` addresses it.

A service account has exactly one membership, in its org. A person has exactly one in the directory and at most one non-removed per org.

Credentials hold no labels of their own. Validate resolves the member's role every time.

`memberships.role` and `organization_invites.role` are nullable `TEXT` slugs. No `CHECK` constraint: the roles document gives them meaning, and identity checks the slug against it on write. `NULL` while roles are off, and on directory memberships. With roles on, an org membership's writes always set a role.

## Runtime

| | |
|--|--|
| Directory | `services/identity/` |
| `service.name` | `identity` |
| `service.namespace` | `identity` |
| Public port | `3000` |
| Internal port | `3001` (`/health/*`, `/metrics`, credential validate, lifecycle events) |
| Database | Plat5 Postgres via `DATABASE_URL` |
| Schema | **`identity`** (service-owned; tables + `schema_migrations`) |
| `APIKEY_BRAND` | default `plat5`. Identity only ([format](#credential-format)) |
| `ROLES_ENABLED` | default `true`. `false` → roles off (logged as a warning). Anything else refuses boot. A boot switch: only the roles themselves change while identity runs. [`roles.md`](roles.md) |
| `CONTROL_PLANE_URL` | required while roles are on. The control plane's internal base URL, e.g. `http://control-plane:5003`. Identity does not listen until it has the roles document. [`control-plane.md`](control-plane.md#internal-feeds) |

Ready probe fails closed (**503** `unhealthy`) when Postgres is unreachable, or roles are on and not loaded.

## Non-goals

- Login UI, password store, JWKS (IdP / Plat5 Auth)
- Global `/service-accounts` (SAs are org-scoped)
- Multi-org service accounts (one SA, one org, one member)
- Org `settings` / config bag
- A list or search of people for customers. `GET /users` is for staff and is not in the customer catalog.
- `user_id` in an org's view of a member, or as the way to add one. Members are added by IdP subject.
- A self-service API to link a second IdP subject (deferred, [`../TODO.md`](../TODO.md))
- SMTP / sending invite email (identity returns a token; the console may send mail)
- Pending member rows. Add by IdP subject and invite redeem both insert an **active** member.
- Resource ACL, FGA, project permissions
- Role names or meanings in identity code. The deployment's roles document names roles and grants their labels.
- More than one role per member, per-org custom roles, or turning roles on or off without a restart
- Caller checks of any kind: a grant cap on roles, on acting on a member, or on minting keys and sessions
- Attribution in identity: who added a member, created an invite, or created a service account. Identity reports what changed; the gateway's audit log records who ([`audit.md`](audit.md))
- Key or session labels, or default-deny on unlabeled routes
- A service account tied to the member who created it
- Auto-publishing these public routes — the operator applies a catalog
- Configurable kinds or version, independent full-prefix env vars, or dual-brand key accept
- The realm or the kind of principal in a credential's prefix (validate returns them)
- Member session refresh, list, revoke, or a TTL env
- A credential table, prefix, or validate endpoint per kind of principal (service-account keys are member keys; user keys, member keys, and sessions are one credential)
- `/organizations/{organization_id}/members/{member_id}/api-keys` (that is acting on another member; service-account keys use the service-account id)
- `user_id` in an organization credential's validate result
- A `labels` field in the session mint body to narrow a session (the session carries the member's role)
