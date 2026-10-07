# Identity service

Plat5 **identity** service: organizations, members, invites, service accounts, API keys, member sessions, and internal auth helpers for the gateway.

Boundary: [`identity-boundary.md`](identity-boundary.md). Errors: [`api-errors.md`](api-errors.md), [`error-copy.md`](error-copy.md). Lists: [`lists.md`](lists.md).

The service is a function of the URL. The path names every id the operation uses. If the handler does not read an id, it is not in the path.

Who may call is the proxy. This service does not know which proxy called, and it does not grow a second policy for a second caller. When it mints a key or a member session, assigns a role, or acts on another member, it reads `X-Plat5-Scopes`, which the gateway sets from the caller credential.

Roles are the deployment's. Identity reads the roles file at boot and resolves a member's role at validate. Model and rules: [`roles.md`](roles.md).

There is no `/api` prefix. The first path segment is the subject.

| Scope | Path | The call is about |
|-------|------|-------------------|
| user | `/users/{user_id}/...` | that person |
| org | `/organizations/{organization_id}/...` | that org |
| member | `/members/{member_id}/...` | that member |

A member lives in one org. `member_id` is enough. Do not also put `organization_id` on a member route. Do not put a user id on an org route.

Public routes are not auto-published. The process still serves them on the public port. Internal validate stays on `INTERNAL_PORT`.

## Glossary

| Term | Meaning |
|------|---------|
| **user** | Platform person. Opaque `user_id` string. Not a table. |
| **organization** | Isolation boundary users and service accounts join. |
| **member** | Org principal. Exactly one of: a **user** or a **service account**. Wire id: `member_id`. |
| **role** | Slug on a member. The deployment's roles file says which labels it grants. Plat5 defines no role names. `NULL` is unrestricted. [`roles.md`](roles.md) |
| **service account** | Non-human identity created **under an organization**. Always has a member row in that org. |
| **api key** | Bearer secret. Either **user-scoped** or **member-scoped**. Optional `scopes` labels: a restricted key (non-null list) must intersect route `required_scopes`; unlabeled routes still admit. A restricted key cannot mint a wider key or session. |
| **member session** | Short-lived credential for one active user member in one org. Opaque token. Not an API key. `scopes` null is unrestricted. A session minted from a restricted user key carries that key's scopes. |
| **membership** | A user's member row in an org, with that org. Not a table. Wire resource for which orgs this person belongs to. |
| **invite** | Org join token (`active` / `redeemed` / `revoked` / `expired`). Redeem inserts an **active** member. The host sends any email. |

## What the service refuses

Illegal states. Not permissions.

- Slug is globally unique.
- A member is exactly one of user or service account.
- One member row per `(organization_id, user_id)`, including `removed`. One member row per service account. A service account lives in exactly one org.
- A remove must leave at least one non-removed member. `active` and `suspended` both count. Service accounts count.
- Invite expiry, use limits, and the conflict on a dead token.
- A key addressed under a user, member, or service account that does not own it is **404**. That is the address, not an access check.
- A service account or member addressed under the wrong org, or whose member is `removed`, is **404**.
- Unknown id is **404**. A removed member is **404**. Empty collection is an empty page.
- A `role` that is not a slug in the roles file, or any `role` when there is no roles file, is **422**.
- A write may not take an org's non-removed members holding `creator_role` from one to zero (**422**).

Validation (**422**) and conflict (**409**) stay where the data is wrong. Identity does not decide who may call. It does refuse to hand out more than the caller holds: a key or session wider than the caller's scopes, a role with labels the caller lacks, or an act on a member whose role holds labels the caller lacks (**403** `INSUFFICIENT_SCOPE`). Grant cap: [`roles.md`](roles.md#grant-cap).

## Public API

Pagination: [`lists.md`](lists.md). `limit`, `starting_after`, `has_more`, sort `id` ascending.

`added_by`, `created_by`, and `created_by_user_id` are not inferred. The proxy sends them in the body when it wants them stored. Omitted or blank means null.

### Memberships

Active **user** memberships only: not service accounts, not `suspended`, not `removed`.

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/users/{user_id}/memberships` | Which orgs this person belongs to |

#### Row

```json
{
  "id": "...",
  "organization": { "id": "...", "name": "Acme", "slug": "acme" },
  "role": "admin",
  "status": "active"
}
```

List body key `memberships`. `id` is the member id. Cursor is that `id`. `role` is `string | null`.

Not a table. A read of `members` joined to `organizations` for this user.

### User API keys

Person credential. Not member keys — those live under the member: `/members/{member_id}/api-keys`.

| Method | Path | Notes |
|--------|------|--------|
| `POST` | `/users/{user_id}/api-keys` | Create; plaintext once — prefix **`{brand}-sk-1-`**. Optional `scopes`. |
| `GET` | `/users/{user_id}/api-keys` | List (no hashes / no secret); echoes `scopes` |
| `DELETE` | `/users/{user_id}/api-keys/{key_id}` | Soft-revoke; idempotent. Other user's key → **404** |

#### Create body

```json
{ "name": "ci", "scopes": ["widgets:read"] }
```

`name` optional (default `Unnamed Key`, max 128). `scopes` optional.

What a stored list means:

| Stored | Route with `required_scopes` | Unlabeled authenticated route |
|--------|------------------------------|-------------------------------|
| SQL `NULL` | skip (allowed) | allowed |
| empty array | **403** | allowed |
| non-empty array | allowed if nonempty intersection | allowed |

Restricted = non-null list (`[]` or labels). JWT and `null` skip the check. Unlabeled = any admitted principal.

Mint cap. The gateway sends the caller credential's scopes on `X-Plat5-Scopes` ([`gateway-contract.md`](gateway-contract.md)). Header absent = unrestricted (a JWT, or a key or session whose scopes are null). Header present = restricted: comma-separated labels, or `[]` when the list is empty.

| Caller | Requested `scopes` | Stored |
|--------|--------------------|--------|
| unrestricted | omitted or `null` | SQL `NULL` |
| unrestricted | `[]` or a list | that list |
| restricted | omitted or `null` | exactly the caller's scopes (never `NULL`) |
| restricted | a list that is a subset | that list |
| restricted | any label the caller does not have | **403** `INSUFFICIENT_SCOPE` |

The 403 message names the missing labels. Explicit `[]` is a subset of every caller, so a restricted caller may mint a key with no labels. It may not mint `NULL`.

Identity does not enforce route `required_scopes`. That check is the gateway's, on routes the operator labeled. Keep redeem unlabeled when it is published — the invitee is not a member yet. Member keys never hit user routes. Gateway: [`gateway-contract.md`](gateway-contract.md), [`routes.md`](routes.md).

The same mint cap applies to member keys, service-account keys, and member sessions.

For a member credential the header is the effective set: the member's role labels intersected with the credential's scopes ([`roles.md`](roles.md)). A key minted with omitted `scopes` by a caller whose role has a label list stores a snapshot of those labels. It does not gain labels the role gains later. [`roles.md`](roles.md#keys-minted-by-a-role-restricted-caller)

Hygiene (422 `VALIDATION_ERROR` on `scopes`): each label `[a-z0-9:._-]+`, max 64 characters, max 32 labels, unique. Create and list echo `scopes` as `string[] | null` (`null` = unrestricted). Never echo the secret except on create (`key`).

Create **201** also includes `"key": "{brand}-sk-1-…"` once.

### Organizations

| Method | Path | Notes |
|--------|------|--------|
| `POST` | `/users/{user_id}/organizations` | Create. Body `{ "name", "slug?" }`. Inserts an **active** member for that user with `creator_role` (`NULL` without a roles file). `added_by` is null. |
| `GET` | `/organizations` | Every organization. |
| `GET` | `/organizations/{organization_id}` | **404** if missing |
| `PATCH` | `/organizations/{organization_id}` | Name and slug. Slug uniqueness stays. |
| `DELETE` | `/organizations/{organization_id}` | Hard delete. Cascades members, invites, service accounts, and keys. Missing → **404**. Not blocked by the last-member rule. |

#### Create body

```json
{ "name": "Acme", "slug": "acme" }
```

`slug` optional (derived from name). Globally unique. IDs are **ULID** strings. `name` max 128 chars.

#### Org response

```json
{
  "id": "...",
  "name": "Acme",
  "slug": "acme",
  "created_at": "...",
  "updated_at": "..."
}
```

### Members

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/organizations/{organization_id}/members` | Non-removed members. Unknown org → **404**. Empty org → empty page. |
| `POST` | `/organizations/{organization_id}/members` | Body `{ "user_id", "role?", "added_by?" }`. Immediate `active`. `role` omitted → `default_role`. Grant cap on the role. |

A member has two addresses, like a service account. Both read and write the same row.

| Address | Who is acting | Published |
|---------|---------------|-----------|
| `/members/{member_id}` | The member, on itself | `member` scope, `/member` |
| `/organizations/{organization_id}/members/{member_id}` | The org, on one of its members | `organization` scope, `/org/members/{member_id}` |

Self address:

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/members/{member_id}` | **404** if missing or `removed` |
| `PATCH` | `/members/{member_id}` | Body `{ "status" }`. `active` or `suspended` only. `removed` → **422**. Already `removed` → **404**. |
| `DELETE` | `/members/{member_id}` | Soft-remove. Already `removed` → **404**. Last non-removed member → **422**. Last `creator_role` holder → **422**. |

The self address does not change `role`, and the grant cap does not apply to it. A member may suspend or remove itself.

Org address:

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/organizations/{organization_id}/members/{member_id}` | **404** if missing, in another org, or `removed` |
| `PATCH` | `/organizations/{organization_id}/members/{member_id}` | Body `{ "status?", "role?" }`, at least one. `status` as on the self address. Same **404** as get. Grant cap on the member's current role and on the new role. Last `creator_role` holder → **422**. |
| `DELETE` | `/organizations/{organization_id}/members/{member_id}` | Soft-remove. Same **404** as get. Grant cap on the member's current role. Last non-removed member → **422**. Last `creator_role` holder → **422**. |

Grant cap and last `creator_role` holder: [`roles.md`](roles.md).

`POST` adds a **user** member (immediately `active`). A non-removed duplicate is **409** `CONFLICT` (`field` is `user_id`). A `removed` row for that user is revived: same member id, `active`, `role` and `added_by` from the body (`role` omitted → `default_role`). Member keys are not revoked on remove, so a revive re-admits those keys.

Service accounts are created via the service-accounts API (member row included). Invites are a separate resource, below.

To replace the last person, add the new member first, then remove the old one. To destroy the org, `DELETE` the organization. That cascades, including the last member.

Suspending the last active member is allowed. The org still has a member.

#### Member response

```json
{
  "id": "...",
  "organization_id": "...",
  "principal": "user",
  "user_id": "...",
  "service_account_id": null,
  "role": "member",
  "status": "active",
  "added_by": null,
  "created_at": "...",
  "updated_at": "..."
}
```

`principal` is `"user"` or `"service_account"`. Exactly one of `user_id` / `service_account_id` is non-null. `role` is `string | null`. `added_by` is null unless sent.

### Invites

Token invites. Membership is created only on redeem, as `active`. The host sends any email.

An invite is `active` while it can still be redeemed. Terminal statuses: `redeemed`, `revoked`, `expired`. Plaintext `token` is stored and returned only while `active`. `token_hash` is always stored (redeem lookup) and kept after the row is terminal.

List, redeem, and revoke expire lazily: if `expires_at` is in the past and status is still `active`, persist `expired` and null `token`.

| Method | Path | Notes |
|--------|------|--------|
| `POST` | `/organizations/{organization_id}/invites` | Body `{ "email?", "role?", "expires_in_seconds?", "max_uses?", "created_by?" }`. Returns `token`. Unknown org → **404**. `role` omitted → `default_role`. Grant cap on the role. |
| `GET` | `/organizations/{organization_id}/invites` | `token` included while `active`. Unknown org → **404**. |
| `DELETE` | `/organizations/{organization_id}/invites/{invite_id}` | Revoke. Idempotent. Status `revoked`, `token` null. Hash stays. |
| `POST` | `/users/{user_id}/invites/redeem` | Body `{ "token" }`. Inserts an **active** member for that user. Already a member on a still-`active` token → **200** idempotent (counts as a use). A `removed` row is revived (same member id). Unknown token → **404** `NOT_FOUND` (no org leak). Redeemed / revoked / expired → **409** `CONFLICT` (`field` is `status`, `value` is the terminal status). |

#### Create body

```json
{ "email": "a@b.com", "role": "member", "expires_in_seconds": 604800, "max_uses": 1, "created_by": "..." }
```

`role` is checked and capped at create, not at redeem. `email` is optional display metadata; it is **not** mailed. `expires_in_seconds` default 7 days, min 60, max 30 days. `max_uses` omitted → 1. JSON `null` → unlimited. `0` and negatives → **422**. `created_by` omitted or blank → null.

Token prefix `inv_`. `token_hash` is SHA-256 hex. `use_count` increments on successful redeem. When `use_count` reaches `max_uses`, status becomes `redeemed` and `token` is nulled. Unlimited (`max_uses` null) stays `active` with `token`.

Redeem copies `created_by` onto the new or revived member's `added_by`, and the invite's `role` onto its `role`. Null stays null. A slug since removed from the roles file is copied as-is and grants nothing. Already a member: redeem does not change the existing role.

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
  "created_by": null,
  "created_at": "..."
}
```

`token` is omitted once the row is terminal.

### Service accounts

Created under an organization. One transaction: service account row + **active** member. Lives in **that org only**.

| Method | Path | Notes |
|--------|------|--------|
| `POST` | `/organizations/{organization_id}/service-accounts` | Body `{ "name", "role?", "created_by_user_id?" }`. Unknown org → **404**. `role` omitted → `default_role`. Grant cap on the role. |
| `GET` | `/organizations/{organization_id}/service-accounts` | Non-removed. Unknown org → **404**. |
| `GET` | `/organizations/{organization_id}/service-accounts/{service_account_id}` | **404** if missing, wrong org, or member `removed` |
| `PATCH` | `/organizations/{organization_id}/service-accounts/{service_account_id}` | Body `{ "name" }`. Same **404** as get. Grant cap on the member's role. |
| `DELETE` | `/organizations/{organization_id}/service-accounts/{service_account_id}` | Soft-removes the member. Same **404** as get. Grant cap on the member's role. Last non-removed member → **422**. Last `creator_role` holder → **422**. |

The member row's `added_by` is null. `created_by_user_id` is null unless sent.

Lifecycle and role are the member row. Suspend, re-enable, or change role at the org address, `PATCH /organizations/{organization_id}/members/{member_id}`, with the service account's `member_id`. A removed service account is not addressable here. Re-entry is not a service-account create; the row remains.

#### Service account response

```json
{
  "id": "...",
  "organization_id": "...",
  "member_id": "...",
  "name": "deploy-bot",
  "role": "member",
  "status": "active",
  "created_by_user_id": null,
  "created_at": "...",
  "updated_at": "..."
}
```

`status` and `role` are the joined member's (`status` is `active` or `suspended`; `role` is `string | null`). `removed` members are not listed and are not returned by id.

### Service account API keys

Org address for that service account's member keys. Not a separate credential. Same table (`member_api_keys`), same plaintext prefix (`{brand}-mk-1-`), same validate endpoint. `/members/{member_id}/api-keys` is the self address. A key created on either path is listed and revoked on both.

Who may call is the proxy, same as creating the service account. The mint cap applies, and so does the grant cap on the service account's role: minting or revoking its keys acts on that member ([`roles.md`](roles.md#grant-cap)).

| Method | Path | Notes |
|--------|------|--------|
| `POST` | `/organizations/{organization_id}/service-accounts/{service_account_id}/api-keys` | Create; plaintext once — prefix **`{brand}-mk-1-`**. Body same as member keys (`name`, optional `scopes`), including the mint cap. |
| `GET` | `/organizations/{organization_id}/service-accounts/{service_account_id}/api-keys` | List. Collection key `keys`. Echoes `scopes`, never the secret. |
| `DELETE` | `/organizations/{organization_id}/service-accounts/{service_account_id}/api-keys/{key_id}` | Soft-revoke. Idempotent. Key not under this service account → **404**. |

Missing service account, wrong org, or member `removed` → **404**, same as get. A `suspended` service account is addressable. Validate still rejects a key whose member is not `active`.

Published on `organization` scope as `/org/service-accounts/{service_account_id}/api-keys`. The client names the service account, not `member_id`.

### Member API keys

Keys that authenticate **as a member**. Different product from `/users/{user_id}/api-keys`: the parent path is the member. When the member is a service account, the service-account path reads and writes these same rows.

| Method | Path | Notes |
|--------|------|--------|
| `POST` | `/members/{member_id}/api-keys` | Create; plaintext once — prefix **`{brand}-mk-1-`**. Optional `scopes` (same semantics as user keys, including the mint cap). |
| `GET` | `/members/{member_id}/api-keys` | List (echoes `scopes`, never the secret) |
| `DELETE` | `/members/{member_id}/api-keys/{key_id}` | Soft-revoke. Idempotent. Other member's key → **404** |

Missing or `removed` member → **404**. A `suspended` member is addressable. Validate still rejects a key whose member is not `active`.

Member keys are a **separate product surface** from user keys: different table (`member_api_keys`), different plaintext prefix (`{brand}-mk-1-` vs `{brand}-sk-1-`), different validate endpoint. Both use the `X-API-Key` header. Hashing at rest is SHA-256 hex. List may include revoked keys (`revoked_at` set).

### Member sessions

Short-lived credential for one **active user member** in one org. Not a member API key: different table (`member_sessions`), different plaintext prefix (`{brand}-ms-1-`), different validate endpoint. No name, no list, no revoke. Minting another session does not invalidate older ones. They expire.

TTL is **1 hour**. Not boot config.

Who may call is the proxy. A user JWT and an unrestricted user API key are the same proof: the gateway does not send `X-Plat5-Scopes`. A restricted user API key does. Identity does not otherwise see which credential it was. The path `user_id` is the subject. A service account does not mint a session. It uses a member key.

| Method | Path | Notes |
|--------|------|--------|
| `POST` | `/users/{user_id}/organizations/{organization_id}/session` | No body. **201** returns the token once. Not an active member of that org (missing, `suspended`, `removed`, unknown org) → **404**. |

Empty `user_id` or `organization_id` → **422**. `user_id` longer than 128 → **422**.

There is no `scopes` field on the request. The session stores the caller's scopes: exactly its own when restricted, `null` when unrestricted. The member's role is not stored on the session. Validate intersects it every time, so a role change reaches existing sessions.

#### Mint response

```json
{
  "token": "{brand}-ms-1-…",
  "expires_at": "...",
  "member_id": "...",
  "organization_id": "...",
  "role": "admin",
  "scopes": ["org:write", "org:members:write", "org:service-accounts:write"]
}
```

No `user_id`. `role` is `string | null`. `scopes` is `string[] | null`: the effective set at mint, the same value validate would return now (role labels intersected with the stored scopes). A console can use it to show or hide actions. Hashing at rest is SHA-256 hex. Plaintext is returned once.

### Roles

The deployment's roles, from the roles file. Same list for every org today. Shape and rules: [`roles.md`](roles.md#listing-roles).

| Method | Path | Notes |
|--------|------|--------|
| `GET` | `/organizations/{organization_id}/roles` | `{ "roles": [{ "slug", "scopes" }], "creator_role", "default_role" }`. Sorted by slug. Not paginated. Unknown org → **404**. No roles file → `roles: []`, both fields `null`. |

### API key brand

`APIKEY_BRAND` is boot config on **identity and the gateway**. Same value on both. Unset → `plat5`. Empty-but-set → refuse boot.

| | |
|--|--|
| Brand | `[a-z][a-z0-9]*`, max 32. Lowercase only — no folding. |
| User wire prefix | `{brand}-sk-1-` |
| Member wire prefix | `{brand}-mk-1-` |
| Member session wire prefix | `{brand}-ms-1-` |

`sk`, `mk`, `ms`, and `1` are fixed. One brand for the process, read at boot.

Changing brand does not rewrite stored secrets. Old plaintext no longer matches; those rows cannot authenticate.

## Status

Status is whether the member is admitted, not what they are allowed to do.

| Status | |
|--------|--|
| `active` | Admitted |
| `suspended` | Not admitted |
| `removed` | Soft-deleted; not listed; not addressable |

`PATCH` accepts `active` or `suspended` only. `removed` is `DELETE`.

## Internal APIs

Not published on the gateway. Served only on **`INTERNAL_PORT`**. Optional `INTERNAL_AUTH_TOKEN` → header `X-Plat5-Internal-Token` (constant-time compare). Unset = network-trust only (dev). These are lookups for a proxy, not checks on the resource handlers. Do not fold them into the resource URLs.

Gateway env: `USER_APIKEY_VALIDATE_URL`, `MEMBER_APIKEY_VALIDATE_URL`, `MEMBER_SESSION_VALIDATE_URL`, same `INTERNAL_AUTH_TOKEN`, same `APIKEY_BRAND`. All three URLs are required to boot. `APIKEY_CACHE_TTL_SECS` covers user keys, member keys, and sessions.

There is **no** combined key validate and **no** `key_type`. Gateway picks the endpoint from the credential’s wire prefix before calling identity.

### User API key validate

```
POST /internal/user-keys/validate
Content-Type: application/json
X-Plat5-Internal-Token: <INTERNAL_AUTH_TOKEN>   # when token is set

{ "key": "{brand}-sk-1-…" }
```

| Result | Response |
|--------|----------|
| Valid user key | **200** `{ "valid": true, "user_id": "…", "scopes": null }` or `"scopes": ["widgets:read"]` or `"scopes": []` |
| Wrong prefix / missing / revoked / unknown | **200** `{ "valid": false }` |

`scopes` is `string[] | null`. `null` = unrestricted (skip `required_scopes`). `[]` = restricted, no labels (**403** on routes with `required_scopes`; unlabeled still admit).

Gateway: prefix `{brand}-sk-1-` → this URL. Subject = `user_id` (same as JWT for scope checks). Gateway caches valid hits and invalid keys (`APIKEY_CACHE_TTL_SECS`, default 300s). Revoke is visible at the edge when the TTL expires. Transport / 503 are not cached. Contract: [`gateway-contract.md`](gateway-contract.md).

### Member API key validate

```
POST /internal/member-keys/validate
Content-Type: application/json
X-Plat5-Internal-Token: <INTERNAL_AUTH_TOKEN>   # when token is set

{ "key": "{brand}-mk-1-…" }
```

| Result | Response |
|--------|----------|
| Valid active member key | **200** `{ "valid": true, "member_id": "…", "organization_id": "…", "scopes": null }` (`scopes` same meaning as user keys) |
| Wrong prefix / missing / revoked / inactive member / unknown | **200** `{ "valid": false }` |

`scopes` is the effective set: the member's role labels intersected with the key's stored scopes ([`roles.md`](roles.md#resolution)). Role resolution happens here. The response carries no role.

Gateway: prefix `{brand}-mk-1-` → this URL. **`organization` and `member` scopes** (see gateway contract). Same cache as user keys and sessions (`APIKEY_CACHE_TTL_SECS`).

| Validate outcome (any endpoint) | Client |
|------------------------------------|--------|
| `valid: false` or unknown prefix | **401** `UNAUTHORIZED` |
| transport / non-2xx / `valid: true` missing required fields | **503** `SERVICE_UNAVAILABLE` |

### Member session validate

```
POST /internal/member-sessions/validate
Content-Type: application/json
X-Plat5-Internal-Token: <INTERNAL_AUTH_TOKEN>   # when token is set

{ "token": "{brand}-ms-1-…" }
```

| Result | Response |
|--------|----------|
| Unexpired session, member `active` | **200** `{ "valid": true, "member_id": "…", "organization_id": "…", "scopes": null }` or `"scopes": ["profile:read"]` or `"scopes": []` |
| Wrong prefix / missing / expired / member not `active` / unknown | **200** `{ "valid": false }` |

No `user_id`. `scopes` is `string[] | null`, same meaning as a key, and the same effective set: the member's role labels intersected with the session's stored scopes. `null` skips route `required_scopes`: the role is unrestricted and the session was minted by an unrestricted caller. A non-null list is restricted. Gateway: prefix `{brand}-ms-1-` → `MEMBER_SESSION_VALIDATE_URL`. **`organization` and `member` scopes.** Same cache TTL as API keys (`APIKEY_CACHE_TTL_SECS`).

## Data model (logical)

```
organizations
members
  user_id XOR service_account_id
  role?, status, added_by, …
  unique (organization_id, user_id) where user_id is not null
  unique (service_account_id) where service_account_id is not null
service_accounts
  organization_id
  name, created_by_user_id, …
organization_invites   -- token while active; token_hash always
  organization_id, email?, role?, token?, token_hash, status, max_uses, use_count, expires_at, created_by?, …

user_api_keys          -- person credentials; wire {brand}-sk-1-
  user_id, name, key_prefix, key_hash, scopes, revoked_at, …
member_api_keys        -- member credentials; wire {brand}-mk-1-
  member_id, name, key_prefix, key_hash, scopes, revoked_at, …
member_sessions        -- short-lived user-member credential; wire {brand}-ms-1-
  member_id, token_prefix, token_hash, scopes, expires_at, …
```

`scopes` is `TEXT[]` on key tables and on `member_sessions`. SQL `NULL` = unrestricted. Empty array = restricted, no labels (same rule as mint). User key validate returns that value. Member key and session validate return it intersected with the member's role labels. A session minted before this column existed stays `NULL` (unrestricted), which is what mint used to store.

Independent tables, independent packages (`userkeys` / `memberkeys` / `sessions`), independent validate endpoints. Not one polymorphic credential system.

No IdP user table and no FK to an external directory. `user_id` values are opaque strings.

`organization_invites.created_by` is nullable. `members.added_by` and `service_accounts.created_by_user_id` are nullable.

`members.role` and `organization_invites.role` are nullable `TEXT` slugs. No `CHECK` constraint: the roles file gives them meaning, and identity checks the slug against it on write. `NULL` is unrestricted. Rows from before roles stay `NULL`.

## Runtime

| | |
|--|--|
| Directory | `services/identity/` |
| `service.name` | `identity` |
| `service.namespace` | `identity` |
| Public port | `3000` |
| Internal port | `3001` (`/health/*`, `/metrics`, validate) |
| Database | Plat5 Postgres via `DATABASE_URL` |
| Schema | **`identity`** (service-owned; tables + `schema_migrations`) |
| `APIKEY_BRAND` | default `plat5`; same value as gateway |
| `ROLES_FILE` | optional; path to the roles file, read at boot. Unset → no roles. Invalid → refuse boot. [`roles.md`](roles.md) |

Ready probe fails closed (**503** `unhealthy`) when Postgres is unreachable.

## Non-goals

- Login UI, password store, JWKS (IdP / Plat5 Auth)
- Global `/service-accounts` (SAs are org-scoped)
- Multi-org service accounts (one SA, one org, one member)
- Org `settings` / config bag
- A user directory (`GET /users`). Person id is a path parameter, not a collection.
- Platform-owned user rows / IdP account linking (opaque `user_id` only)
- SMTP / sending invite email (identity returns a token; the console may send mail)
- Pending member rows. Add-by-`user_id` and invite redeem both insert an **active** member.
- Resource ACL, FGA, project permissions
- Role names or meanings in identity code. The deployment's roles file names roles and grants their labels.
- More than one role per member, per-org custom roles, or reloading the roles file without a restart
- Caller checks beyond the grant cap (keys, sessions, roles, acting on a member), or inferring `added_by` / `created_by` / `created_by_user_id`
- Key `scopes` as deny-all, or default-deny on unlabeled routes
- Auto-publishing these public routes — the operator applies a catalog
- Configurable `sk` / `mk` / `ms` / `1`, independent full-prefix env vars, or dual-brand key accept
- Member session refresh, list, revoke, or a TTL env
- Putting a member session in `member_api_keys`
- A service-account key table, prefix, or validate endpoint (those keys are member keys)
- `/organizations/{organization_id}/members/{member_id}/api-keys` (that is acting on another member; service-account keys use the service-account id)
- `user_id` on session validate
- A client-supplied `scopes` field on session mint (the session copies the caller credential)
