# Organizations service

Plat5 **organizations** service: org lifecycle, memberships, and internal membership resolve for the gateway.

Boundary: [`identity-boundary.md`](identity-boundary.md). Errors: [`api-errors.md`](api-errors.md).

## Scope and headers

| | |
|--|--|
| Gateway scope | **`user` only** |
| Expect | `X-User-Id` |
| Missing `X-User-Id` | **500** `INTERNAL_ERROR` (gateway bug) |
| Does **not** use | `organization` scope |

Membership **role** is domain data on this service only. Never a gateway-injected header.

## Public API

Prefix: `/api/organizations`

| Method | Path | Notes |
|--------|------|--------|
| `POST` | `/api/organizations` | Create org; creator becomes **owner** (`active`) |
| `GET` | `/api/organizations` | List orgs where caller has **active** membership |
| `GET` | `/api/organizations/{organization_id}` | Active member only |
| `PATCH` | `/api/organizations/{organization_id}` | Admin or owner |
| `DELETE` | `/api/organizations/{organization_id}` | Owner only |
| `GET` | `/api/organizations/{organization_id}/memberships` | Active member |
| `POST` | `/api/organizations/{organization_id}/memberships` | Admin or owner; body `{ user_id, role? }` |
| `GET` | `/api/organizations/{organization_id}/memberships/{membership_id}` | Active member |
| `PATCH` | `/api/organizations/{organization_id}/memberships/{membership_id}` | Role/status; admin/owner (self-leave allowed) |
| `DELETE` | `/api/organizations/{organization_id}/memberships/{membership_id}` | Soft-remove; self or admin/owner |

### Existence policy

Unknown org, non-member, or non-**active** membership on org-scoped resources → **404** `NOT_FOUND` (not 403). Insufficient **role** for an admitted member → **403** `FORBIDDEN`.

### Roles and status

| Role | |
|------|--|
| `member` | Default |
| `admin` | Manage non-owner memberships (not promote/demote/remove owners); update org |
| `owner` | Full admin + delete org + manage other owners |

| Status | |
|--------|--|
| `pending` | Reserved (invites later); not admitted |
| `active` | Admitted |
| `suspended` | Not admitted |
| `removed` | Soft-deleted; not listed |

At least one **active owner** must remain. Sole owner cannot leave, be removed, or be demoted. Only **owners** may change another owner’s role/status or remove them.

### Create body

```json
{ "name": "Acme", "slug": "acme", "settings": {} }
```

`slug` optional (derived from name). Globally unique. IDs are **ULID** strings. `name` max 128 chars. `settings` must be a JSON **object** (max 16 KiB).

### List pagination

`GET` list endpoints accept `limit` (default 50, max 100) and `offset` (default 0). Responses include `has_more`.

### Org response

```json
{
  "id": "...",
  "name": "Acme",
  "slug": "acme",
  "settings": {},
  "created_at": "...",
  "updated_at": "..."
}
```

### Membership response

```json
{
  "id": "...",
  "organization_id": "...",
  "user_id": "...",
  "role": "member",
  "status": "active",
  "invited_by": "...",
  "created_at": "...",
  "updated_at": "..."
}
```

## Internal membership resolve

Not published on the gateway. Served only on **`INTERNAL_PORT`**. Gateway calls for `organization` scope.

```
POST /internal/memberships/resolve
Content-Type: application/json
X-Plat5-Internal-Token: <INTERNAL_AUTH_TOKEN>   # required when token is set

{ "user_id": "...", "organization_id": "..." }
```

### Trust model

| Layer | Behavior |
|-------|----------|
| Path | `/internal/memberships/resolve` |
| Bind | `INTERNAL_PORT` (default `3001`); not host-published in compose |
| Token | Optional `INTERNAL_AUTH_TOKEN` → header `X-Plat5-Internal-Token`. Unset = network-trust only. |
| Gateway | `MEMBERSHIP_RESOLVE_URL` + same `INTERNAL_AUTH_TOKEN` |

Gateway may cache active memberships (`MEMBERSHIP_CACHE_TTL_SECS`, default 300s). Remove/suspend is not edge-instant until TTL expires.

**Hit (200):**

```json
{
  "membership_id": "...",
  "organization_id": "...",
  "user_id": "...",
  "status": "active"
}
```

**Miss:** **404** `NOT_FOUND` (no row, or `removed`).

Response includes `status` but **not** `role`. Gateway admits only when `status === "active"`; any other status → gateway **404**.

## Runtime

| | |
|--|--|
| Directory | `organizations` |
| `service.name` | `organizations` |
| `service.namespace` | `identity` |
| Public port | `3000` |
| Internal port | `3001` (`/health/*`, `/metrics`, membership resolve) |
| Database | Plat5 Postgres via `DATABASE_URL` |
| Schema | **`organizations`** (service-owned; tables + `schema_migrations`) |

Ready probe fails closed (**503** `unhealthy`) when Postgres is unreachable.
