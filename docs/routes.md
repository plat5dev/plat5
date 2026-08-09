# Route Publishing

Publish routes for the Plat5 gateway to discover and proxy traffic.

## Pipeline

```
Service (routes.yml) → route-registry admin API → etcd (identity/gateway/routes/{name}) → Gateway
```

Gateway loads routes from **etcd** at startup and watches for live updates. New services register without restarting the gateway.

Write path details: [`route-registry.md`](route-registry.md).

## etcd Schema

| Key | Value | Written By |
|-----|-------|------------|
| `identity/gateway/routes/{service_name}` | JSON `ServiceConfig` blob | **route-registry** |

Gateway watches the prefix. Create/modify/delete triggers a full route reload.

## Route registry

Long-running service. Validates config, expands `route_prefix`, writes JSON to etcd.

- **Local admin URL:** `http://localhost:5002`
- **Auth:** `Authorization: Bearer <ADMIN_TOKEN>`
- **Apply:** `POST /v1/apply` with a full `routes.yml` body (JSON or YAML)
- **Seed:** identity routes (`api-keys`, `organizations`) on boot from `SEED_ROUTES_DIR`

```bash
curl -sS -X POST http://localhost:5002/v1/apply \
  -H "Authorization: Bearer dev-admin-token" \
  -H "Content-Type: application/yaml" \
  --data-binary @routes.yml
```

Validation is at **apply time**. Malformed config → `422 VALIDATION_ERROR`; nothing written for that apply batch once validation fails pre-write (per-service prepare still rejects bad entries).

After validation, writes are **best-effort per service** (not a multi-key transaction). Mid-batch etcd failure → `503` with `results[]` showing `upserted` / `failed` / `skipped`. See [route-registry.md](route-registry.md).

JSON in etcd (not YAML): registry validates and canonicalizes at write time; gateway deserializes into route types.

### Environment Variables (route-registry)

| Variable | Description | Default |
|----------|-------------|---------|
| `ETCD_URL` | etcd client endpoint | `http://localhost:2379` |
| `ADMIN_TOKEN` | Bearer token for `/v1/*` | Required |
| `SEED_ROUTES_DIR` | Directory of seed YAML files | empty |
| `PORT` | Admin API port | `5002` |
| `INTERNAL_PORT` | Health port | `5003` |

## Config Format (`routes.yml`)

Auth is **scopes** (`public`, `user`, `organization`) — not a flat per-route `auth:` field.

```yaml
services:
  my-service:
    url: my-service:3000
    public:
      routes:
        - path: /public/health
          methods: [GET]

    user:
      routes:
        - path: /api/widgets
          methods: [GET, POST]

        - path: /api/widgets/{id}
          methods: [GET, DELETE]
```

`url` is whatever the **gateway** can reach (cluster DNS, public HTTPS, `host.docker.internal:PORT`, etc.). How you run the process is out of scope for Plat5.

### Fields

| Field | Type | Description |
|-------|------|-------------|
| `services` | `map<string, ServiceConfig>` | Top-level wrapper. Keys are service names. |
| `url` | `string` | Upstream URL (hostname:port or absolute URL the gateway can dial). |
| `public` | `ScopeConfig?` | No authentication. |
| `user` | `ScopeConfig?` | JWT or API key. |
| `organization` | `ScopeConfig?` | JWT/API key + active membership resolve. |
| `route_prefix` | `string?` | Optional on any scope. Registry expands into each `path` before etcd. |
| `organization_param` | `string` | **Required** on `organization` scope — path param name for org id. |
| `routes` | `array<RouteConfig>` | HTTP routes for this scope. |
| `path` | `string` | HTTP path (`/` or starts with `/`). Supports `{param}`. |
| `methods` | `array<string>` | Allowed HTTP methods. |
| `transform` | `object?` | Optional path rewrite (see below). |

A service must define at least one scope. Multiple scopes may be present.

### Path Transforms

```yaml
user:
  routes:
    - path: /api/widgets
      methods: [GET]
      transform:
        path: /widgets
```

When `transform.path` is present, the gateway rewrites the request path before proxying. **`transform.path` is always an absolute upstream path** (not relative to any `route_prefix`).

## Scopes

| Scope | Auth | Identity headers |
|-------|------|------------------|
| `public` | No | none |
| `user` | JWT or API key | `X-User-Id` only |
| `organization` | JWT/API key + active membership | `X-Organization-Id`, `X-Membership-Id` only |

Full header and service rules: [`gateway-contract.md`](gateway-contract.md). Layer boundary and admission errors: [`identity-boundary.md`](identity-boundary.md).

## Gateway Behavior

### Startup

1. Connect to etcd (`ETCD_URL`, default `http://localhost:2379`).
2. Load all keys under `identity/gateway/routes/`.
3. Parse JSON, validate, build `RouteMap`.
4. Fail fast if etcd unreachable — no start without a route registry store.

### Runtime Updates

1. Watch `identity/gateway/routes/`.
2. On any event: **full reload** of all routes (small set; simpler than incremental).
3. Reload failure → keep current routes, log warning.

### Service Health vs Route Existence

Decoupled. Service down → gateway still knows the route → **503**. Missing route (nothing registered that path) → **404**.

## `organization` scope

Business / product APIs that should not own membership storage. Gateway authenticates, resolves **active** membership for the org id in the path, injects org-context headers only.

**Who uses it:** Business APIs only. **organizations** stays on **`user` scope** (membership authority). Admission steps and errors: [`identity-boundary.md`](identity-boundary.md).

### `organization_param`

Required on **`organization` scope**. Names the path parameter for the organization id (usually `organization_id`).

Registry validation:

- `organization_param` required for `organization` scope
- Every expanded route path must include `{<organization_param>}`
- Membership resolve mandatory for every match (no public/unauthenticated org routes)

### `route_prefix` (optional, any scope)

Joined with each route `path` so configs stay short.

**Join rule:** path must be `/` (exactly the prefix) or start with `/`. Empty `path` invalid. When path is `/`, full path is the prefix with trailing slashes stripped.

**Expand site (locked):** Registry expands `route_prefix` + `path` **before** writing etcd. Registry stores **full paths only** — one expand site, no gateway/registry drift.

`transform.path` remains an absolute upstream path (not relative to `route_prefix`).

### Examples

#### organizations service — `user` scope only

```yaml
services:
  organizations:
    url: organizations:3000
    user:
      route_prefix: /api/organizations
      routes:
        - path: /
          methods: [POST, GET]
        - path: /{organization_id}
          methods: [GET, PATCH, DELETE]
        - path: /{organization_id}/memberships
          methods: [GET, POST]
        - path: /{organization_id}/memberships/{membership_id}
          methods: [GET, PATCH, DELETE]
```

#### Business service — `organization` scope

```yaml
services:
  projects:
    url: projects:3000
    organization:
      route_prefix: /api/organizations/{organization_id}/projects
      organization_param: organization_id
      routes:
        - path: /
          methods: [GET, POST]
        - path: /{project_id}
          methods: [GET, PATCH, DELETE]
```

## Real Examples

### API Keys Service

`services/api-keys/routes.yml`:

```yaml
services:
  api-keys:
    url: api-keys:3000
    user:
      routes:
        - path: /api/keys
          methods: [GET, POST]

        - path: /api/keys/{id}
          methods: [DELETE]
```

## Validation Rules

Registry validates **before etcd**. Gateway validates again at load:

- `path` not empty; must be `/` or start with `/`
- `methods` not empty
- At least one scope (`public`, `user`, and/or `organization`)
- `organization` scope requires `organization_param`; every expanded org path includes `{param}`
- `route_prefix` join rules at registry; etcd stores full paths only
- Duplicate service names in merged registry: first wins, warning
- Malformed JSON values skipped with warning; other routes continue

## Platform Integrity

Auth delegation: [`gateway-contract.md`](gateway-contract.md). Layer split: [`identity-boundary.md`](identity-boundary.md). Admin write path: [`route-registry.md`](route-registry.md).
