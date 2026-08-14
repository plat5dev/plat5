# Identity service

Plat5 **identity** service: organizations, members, service accounts, API keys, and internal auth helpers for the gateway.

Boundary: [`identity-boundary.md`](identity-boundary.md). Errors: [`api-errors.md`](api-errors.md), [`error-copy.md`](error-copy.md). Gateway: [`gateway-contract.md`](gateway-contract.md).

## Scope and headers

| | |
|--|--|
| Gateway scope (all public routes below) | **`user` only** |
| Expect | `X-User-Id` |
| Missing `X-User-Id` | **500** `INTERNAL_ERROR` (gateway bug) |
| Does **not** use | `organization` scope |

This service is the **membership authority**. It must not sit behind gateway org admission into itself. It enforces member and role rules in-process from `X-User-Id` + path.

Member **role** is domain data here only. Never a gateway-injected header. Org-scope business services do not receive role and have no API to load it.

Public routes below are **not** auto-published. Apply `services/identity/routes.yml` (or a subset) via the route-registry. Internal validate/resolve stay on `INTERNAL_PORT` regardless.

## Glossary

| Term | Meaning |
|------|---------|
| **user** | Platform person. Opaque `user_id` string from the gateway (`X-User-Id` / IdP claim). No user directory or profile store in this service. |
| **organization** | Isolation boundary users and service accounts join. |
| **member** | Org principal. Exactly one of: a **user** or a **service account**. Wire id: `member_id`. |
| **service account** | Non-human identity created **under an organization**. Always has a member row in that org. |
| **api key** | Bearer secret. Either **user-scoped** or **member-scoped**. |

## Public API

### User API keys

No `/api/users` collection and no `/me`. Clients already know `user_id` from the IdP token. Path `user_id` must equal `X-User-Id` or → **404** `NOT_FOUND`.

| Method | Path | Notes |
|--------|------|--------|
| `POST` | `/api/users/{user_id}/api-keys` | Create; plaintext once — prefix **`plat5-sk-1-`** |
| `GET` | `/api/users/{user_id}/api-keys` | List (no hashes); `limit` / `offset` / `has_more` |
| `DELETE` | `/api/users/{user_id}/api-keys/{key_id}` | Soft-revoke; idempotent |

### Organizations

Prefix: `/api/organizations`

| Method | Path | Notes |
|--------|------|--------|
| `POST` | `/api/organizations` | Create; caller becomes **owner** member (`active`) |
| `GET` | `/api/organizations` | List orgs where caller has **active** membership |
| `GET` | `/api/organizations/{organization_id}` | Active member |
| `PATCH` | `/api/organizations/{organization_id}` | Admin or owner |
| `DELETE` | `/api/organizations/{organization_id}` | Owner only |

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
| `GET` | `/api/organizations/{organization_id}/members` | Active member |
| `POST` | `/api/organizations/{organization_id}/members` | Admin or owner; add **user** — body `{ "user_id", "role?" }`. Target `user_id` must already be known (IdP id). No invites. |
| `GET` | `/api/organizations/{organization_id}/members/{member_id}` | Active member |
| `PATCH` | `/api/organizations/{organization_id}/members/{member_id}` | Role/status; admin/owner (self-leave allowed for humans) |
| `DELETE` | `/api/organizations/{organization_id}/members/{member_id}` | Soft-remove; self or admin/owner |

`POST` adds a **user** member only (immediately `active`). Service accounts are created via the service-accounts API (member row included). There is no invite or pending state.

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
  "added_by": "...",
  "created_at": "...",
  "updated_at": "..."
}
```

`principal` is `"user"` or `"service_account"`. Exactly one of `user_id` / `service_account_id` is non-null.

### Service accounts

Created under an organization. One transaction: service account row + **active** member (default role `member`). Lives in **that org only**. Service accounts **cannot** be `owner`.

| Method | Path | Notes |
|--------|------|--------|
| `POST` | `/api/organizations/{organization_id}/service-accounts` | Admin or owner; body `{ "name" }` |
| `GET` | `/api/organizations/{organization_id}/service-accounts` | Active member |
| `GET` | `/api/organizations/{organization_id}/service-accounts/{service_account_id}` | Active member |
| `PATCH` | `/api/organizations/{organization_id}/service-accounts/{service_account_id}` | Admin or owner; name / disable |
| `DELETE` | `/api/organizations/{organization_id}/service-accounts/{service_account_id}` | Admin or owner; disables SA and removes member |

#### Service account response

```json
{
  "id": "...",
  "organization_id": "...",
  "member_id": "...",
  "name": "deploy-bot",
  "disabled_at": null,
  "created_by_user_id": "...",
  "created_at": "...",
  "updated_at": "..."
}
```

### Member API keys

Keys that authenticate **as a member** (org context). Used for automation / S2S on `organization` scope routes.

| Method | Path | Notes |
|--------|------|--------|
| `POST` | `/api/organizations/{organization_id}/members/{member_id}/api-keys` | Create; plaintext once — prefix **`plat5-mk-1-`** |
| `GET` | `/api/organizations/{organization_id}/members/{member_id}/api-keys` | List |
| `DELETE` | `/api/organizations/{organization_id}/members/{member_id}/api-keys/{key_id}` | Soft-revoke |

**Who may manage keys**

| Member principal | Create / list / revoke |
|------------------|------------------------|
| `user` (human) | That user (self) or org admin/owner |
| `service_account` | Org admin/owner only |

Member keys are a **separate product surface** from user keys: different table (`member_api_keys`), different plaintext prefix (`plat5-mk-1-` vs `plat5-sk-1-`), different validate endpoint. Same header name (`X-API-Key`) only. Hashing at rest is SHA-256 hex in both cases by coincidence, not a shared module requirement. List may include revoked keys (`revoked_at` set).

## Roles and status

| Role | |
|------|--|
| `member` | Default |
| `admin` | Manage non-owner members and service accounts; update org; not promote/demote/remove owners |
| `owner` | Full admin + delete org + manage other owners. **Humans only.** |

| Status | |
|--------|--|
| `active` | Admitted |
| `suspended` | Not admitted |
| `removed` | Soft-deleted; not listed |

At least one **active owner** must remain. Sole owner cannot leave, be removed, or be demoted. Only **owners** may change another owner’s role/status or remove them.

### Existence and authz policy

| Case | HTTP / code |
|------|-------------|
| Unknown org, non-member, or non-**active** member on org resources | **404** `NOT_FOUND` |
| Active member, insufficient **role** | **403** `FORBIDDEN` |
| Path `user_id` ≠ `X-User-Id` on user key routes | **404** `NOT_FOUND` |

## Pagination

List endpoints accept `limit` (default 50, max 100) and `offset` (default 0). Responses include `has_more`.

## Internal APIs

Not published on the gateway. Served only on **`INTERNAL_PORT`**. Optional `INTERNAL_AUTH_TOKEN` → header `X-Plat5-Internal-Token` (constant-time compare). Unset = network-trust only (dev).

Gateway env: `USER_APIKEY_VALIDATE_URL`, `MEMBER_APIKEY_VALIDATE_URL` (when member-key admission is on), `MEMBER_RESOLVE_URL`, same `INTERNAL_AUTH_TOKEN`.

There is **no** combined key validate and **no** `key_type`. Gateway picks the endpoint from the key’s wire prefix before calling identity.

### User API key validate

```
POST /internal/user-keys/validate
Content-Type: application/json
X-Plat5-Internal-Token: <INTERNAL_AUTH_TOKEN>   # when token is set

{ "key": "plat5-sk-1-…" }
```

| Result | Response |
|--------|----------|
| Valid user key | **200** `{ "valid": true, "user_id": "…" }` |
| Wrong prefix / missing / revoked / unknown | **200** `{ "valid": false }` |

Gateway: prefix `plat5-sk-1-` → this URL. Subject = `user_id` (same as JWT for scope checks). Cache: `APIKEY_CACHE_TTL_SECS` (default 300s).

### Member API key validate

```
POST /internal/member-keys/validate
Content-Type: application/json
X-Plat5-Internal-Token: <INTERNAL_AUTH_TOKEN>   # when token is set

{ "key": "plat5-mk-1-…" }
```

| Result | Response |
|--------|----------|
| Valid active member key | **200** `{ "valid": true, "member_id": "…", "organization_id": "…" }` |
| Wrong prefix / missing / revoked / inactive member / unknown | **200** `{ "valid": false }` |

Gateway: prefix `plat5-mk-1-` → this URL. **organization** scope only (see gateway contract). Does not use member resolve.

| Validate outcome (either endpoint) | Client |
|------------------------------------|--------|
| `valid: false` or unknown prefix | **401** `UNAUTHORIZED` |
| transport / non-2xx / `valid: true` missing required fields | **503** `SERVICE_UNAVAILABLE` |

### Member resolve

Used when the credential is a **user** (JWT or user API key) on `organization` scope.

```
POST /internal/members/resolve
Content-Type: application/json
X-Plat5-Internal-Token: <INTERNAL_AUTH_TOKEN>   # when token is set

{ "user_id": "...", "organization_id": "..." }
```

**Hit (200):**

```json
{
  "member_id": "...",
  "organization_id": "...",
  "user_id": "...",
  "status": "active"
}
```

**Miss:** **404** `NOT_FOUND` (no row, or `removed`).

Response includes `status` but **not** `role`. Gateway admits only when `status === "active"`; any other status → gateway **404**.

Gateway may cache active resolves (`MEMBER_CACHE_TTL_SECS`, default 300s). Remove/suspend is not edge-instant until TTL expires.

Member API keys do **not** use this endpoint for admission: validate already returns `member_id` + `organization_id`.

## Data model (logical)

```
organizations
members
  user_id XOR service_account_id
  role, status, added_by, …
service_accounts
  organization_id
  name, disabled_at, created_by_user_id, …

user_api_keys          -- person credentials (user scope); wire plat5-sk-1-
  user_id, name, key_prefix, key_hash, revoked_at, …

member_api_keys        -- org principal credentials (org scope); wire plat5-mk-1-
  member_id, name, key_prefix, key_hash, revoked_at, …
```

Independent tables, independent packages (`userkeys` / `memberkeys`), independent validate endpoints. Not one polymorphic key system.

No IdP user table and no FK to an external directory. `user_id` values are opaque strings from the gateway.

## Runtime

| | |
|--|--|
| Directory | `services/identity/` |
| `service.name` | `identity` |
| `service.namespace` | `identity` |
| Public port | `3000` |
| Internal port | `3001` (`/health/*`, `/metrics`, validate, resolve) |
| Database | Plat5 Postgres via `DATABASE_URL` |
| Schema | **`identity`** (service-owned; tables + `schema_migrations`) |

Ready probe fails closed (**503** `unhealthy`) when Postgres is unreachable.

## Non-goals

- Login UI, password store, JWKS (IdP / Plat5 Auth)
- Global `/service-accounts` (SAs are org-scoped)
- Multi-org service accounts (one SA, one org, one member)
- Org `settings` / config bag
- `GET /api/users` or `/api/users/me`
- Platform-owned user rows / IdP account linking (opaque `user_id` only)
- Invites, email, or pending membership
- Resource ACL, FGA, project permissions
- Gateway `organization` scope on this service’s public routes
- Auto-publishing these public routes — the operator applies the catalog (`routes.yml`)
