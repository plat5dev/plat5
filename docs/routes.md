# Route Publishing

Publish routes for the Plat5 gateway to discover and proxy traffic.

## Pipeline

```
Service (routes.yml) → route-registry (Postgres desired state + revision)
                    → etcd (edge/gateway/routes/{name}) → Gateway
```

Postgres is the source of truth. etcd is the live projection the gateway watches. Apply while gateways are down; they pick up the map on watch/reconnect.

Write path details: [`route-registry.md`](route-registry.md).

## etcd Schema

| Key | Value | Written By |
|-----|-------|------------|
| `edge/gateway/routes/{service_name}` | JSON `ServiceConfig` blob | **route-registry** (projection) |

Gateway watches the prefix. Create/modify/delete triggers a full route reload.

## Route registry

Long-running service. Validates config, expands `route_prefix` and nested `methods` maps, records a revision in Postgres, projects current JSON to etcd.

- **Local admin URL:** `http://localhost:5002`
- **Auth:** `Authorization: Bearer <ADMIN_TOKEN>`
- **Apply:** `POST /v1/apply` with a `routes.yml` body (JSON or YAML) — **upsert** of the services in the file (not a full-map prune)
- **Identity:** public identity routes are operator-owned. Catalog: [`services/identity/routes.yml`](../services/identity/routes.yml). Apply it (or a subset). Dev compose may seed missing services from that file on first boot; it does not overwrite. Prod does not seed.

```bash
curl -sS -X POST http://localhost:5002/v1/apply \
  -H "Authorization: Bearer dev-admin-token" \
  -H "Content-Type: application/yaml" \
  --data-binary @routes.yml
```

Validation is at **apply time**. Malformed config → `422 VALIDATION_ERROR`; nothing written.

After validation, all services in the batch commit in **one Postgres transaction** (each service gets a new revision). etcd projection follows; a reconciler retries if a put fails. `200` means desired state is recorded.

JSON in etcd (not YAML): registry validates and canonicalizes at write time; gateway deserializes into route types. Nested `methods` maps are expanded at apply so etcd `methods` is always a string array.

### Environment Variables (route-registry)

| Variable | Description | Default |
|----------|-------------|---------| 
| `ETCD_URL` | etcd client endpoint | `http://localhost:2379` |
| `DATABASE_URL` | Postgres (schema `routes`) | Required |
| `ADMIN_TOKEN` | Bearer token for `/v1/*` | Required |
| `SEED_ROUTES_DIR` | Optional; upsert **missing** services from YAML (dev) | empty |
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
          required_scopes: [widgets:read]

        - path: /api/widgets/{id}
          methods: [GET, DELETE]
          rate_limit:
            requests: 30
            window_seconds: 60
```

Same path, different per-verb `required_scopes` / `rate_limit` — nested `methods` map (expanded at apply into one etcd row per verb):

```yaml
        - path: /features
          methods:
            GET:
              required_scopes: [org:read]
            POST:
              required_scopes: [org:write]
              rate_limit:
                requests: 100
                window_seconds: 1
```

`url` is whatever the **gateway** can reach (cluster DNS, public HTTPS, `host.docker.internal:PORT`, etc.). How you run the process is out of scope for Plat5.

### Fields

| Field | Type | Description |
|-------|------|-------------|
| `services` | `map<string, ServiceConfig>` | Top-level wrapper. Keys are service names. |
| `url` | `string` | Upstream URL (hostname:port or absolute URL the gateway can dial). |
| `public` | `ScopeConfig?` | No authentication. |
| `user` | `ScopeConfig?` | JWT or **user** API key. |
| `organization` | `ScopeConfig?` | JWT / user API key + member resolve, or **member** API key. |
| `route_prefix` | `string?` | Optional on any scope. Registry expands into each `path` before etcd. |
| `organization_param` | `string` | **Required** on `organization` scope — path param name for org id. |
| `routes` | `array<RouteConfig>` | HTTP routes for this scope. |
| `path` | `string` | HTTP path (`/` or starts with `/`). Supports `{param}`. |
| `methods` | `array<string>` \| `map<string, MethodConfig>` | List form: allowed HTTP methods. Map form: per-verb config (see below). Do not mix list and map on the same route (`422`). |
| `transform` | `object?` | Optional path rewrite (see below). Route-level only — not per-method. |
| `required_scopes` | `string[]?` | Optional. Omitted = any admitted principal. If set, a **restricted** API key must share at least one label. JWTs and unrestricted keys skip. Validated at apply. Route-level value applies only to the flat methods list. |
| `rate_limit` | `false` \| `{requests, window_seconds}` \| omitted | Omitted **inherits** gateway fallback (never silent unlimited). `false` opts out (unlimited). Object overrides. Limiter subject follows route scope (`public`→ip, `user`→user, `organization`→org). Route-level value applies only to the flat methods list. |

A service must define at least one scope. Multiple scopes may be present.

There is no `allowed_services` field.

### `methods`

Two forms. Do not mix them on the same route (`422`).

**Flat list** (unchanged). Optional route-level `required_scopes` / `rate_limit` apply to every method in the list. Templates that do not set scopes keep using this.

```yaml
- path: /features
  methods: [GET, POST]
  required_scopes: [org:read]
  rate_limit: { requests: 60, window_seconds: 60 }
```

**Nested map** (new). Each key is an uppercase HTTP verb (`GET`, `POST`, `PUT`, `PATCH`, `DELETE`, `HEAD`, `OPTIONS`). The body may set `required_scopes` and/or `rate_limit` for that verb only. An empty body (`GET:` or `GET: {}`) means that method with no extra constraints. An empty methods map is rejected.

```yaml
- path: /features
  methods:
    GET:
      required_scopes: [org:read]
    POST:
      required_scopes: [org:write]
      rate_limit:
        requests: 100
        window_seconds: 1
```

Nested maps are an **apply-time YAML convenience**. Registry `prepare_for_registry` / prefix expand turns each verb into its own `RouteConfig` row (same `path`, `methods: [THAT_VERB]`, `required_scopes` / `rate_limit` taken from that method entry). `transform` stays on the path. After expand, etcd JSON keeps today's shape: `methods` is always a string array. Gateway matching still treats `methods` as `Vec<String>`. Duplicate `path`+method after expand → `422`.

Labels are opaque. There is no grant/implication graph: `org:write` does not imply `org:read`.

### `required_scopes`

Labels follow the same hygiene as key mint: `[a-z0-9:._-]+`, max 64 chars, max 32, unique, non-empty list if present.

After match + admission: if the route has `required_scopes` **and** the credential is an API key with a non-null scopes list, the two lists must have a nonempty intersection or the gateway returns **403** `FORBIDDEN` (existing envelope). JWT and unrestricted keys (`scopes: null`) skip.

### `rate_limit`

Applies to **all admitted** routes (JWT and API key), in-process per gateway instance (no Redis).

| YAML | Effect |
|------|--------|
| omitted | Inherit `RATE_LIMIT_REQUESTS` / `RATE_LIMIT_WINDOW_SECONDS`. `0` requests = unlimited fallback. |
| `false` | Unlimited for this route |
| `{requests, window_seconds}` | Override. `requests` and `window_seconds` must be > 0. |

Limiter subject follows route scope: `public`→`ip`, `user`→`user`, `organization`→`org` (`Admission::Organization.organization_id`, including SA/member keys).

Exceed → **429** `RATE_LIMITED`, `Retry-After`, `details.retry_after_seconds`. See [`api-errors.md`](api-errors.md).

A separate failed-auth IP limiter (`RATE_LIMIT_AUTH_FAILURE_*`) covers unadmitted 401s and unmatched 404s. It is not per-route.

### Path Transforms

```yaml
user:
  routes:
    - path: /api/widgets
      methods: [GET]
      transform:
        path: /widgets
```

When `transform.path` is present, the gateway rewrites the request path before proxying. **`transform.path` is always an absolute upstream path** (not relative to any `route_prefix`). `transform` is per path, not per method.

## Scopes

| Scope | Auth | Identity headers |
|-------|------|------------------|
| `public` | No | none |
| `user` | JWT or user API key | `X-User-Id` only |
| `organization` | JWT / user API key + active member, or member API key | `X-Organization-Id`, `X-Member-Id` only |

Full header and service rules: [`gateway-contract.md`](gateway-contract.md). Layer boundary and admission errors: [`identity-boundary.md`](identity-boundary.md).

## Gateway Behavior

### Startup

1. Connect to etcd (`ETCD_URL`, default `http://localhost:2379`).
2. Load all keys under `edge/gateway/routes/`.
3. Parse JSON, validate, build `RouteMap`.
4. Fail fast if etcd unreachable — no start without a route registry store.

### Runtime Updates

1. Watch `edge/gateway/routes/`.
2. On any event: **full reload** of all routes (small set; simpler than incremental).
3. Reload failure → keep current routes, log warning.

### Service Health vs Route Existence

Decoupled. Service down → gateway still knows the route → **503**. Missing route (nothing registered that path) → **404**.

## `organization` scope

Business / product APIs that should not own membership storage. Gateway authenticates, admits an **active** member for the org id in the path, injects org-context headers only.

**Who uses it:** Business APIs only. **identity** stays on **`user` scope** (membership authority). Admission steps and errors: [`identity-boundary.md`](identity-boundary.md).

### `organization_param`

Required on **`organization` scope**. Names the path parameter for the organization id (usually `organization_id`).

Registry validation:

- `organization_param` required for `organization` scope
- Every expanded route path must include `{<organization_param>}`
- Member admission mandatory for every match (no public/unauthenticated org routes)

### `route_prefix` (optional, any scope)

Joined with each route `path` so configs stay short.

**Join rule:** path must be `/` (exactly the prefix) or start with `/`. Empty `path` invalid. When path is `/`, full path is the prefix with trailing slashes stripped.

**Expand site (locked):** Registry expands `route_prefix` + `path` **and nested `methods` maps** **before** writing etcd. Registry stores **full paths only** and **list-form `methods` only** — one expand site, no gateway/registry drift.

`transform.path` remains an absolute upstream path (not relative to `route_prefix`).

### Examples

#### identity service — `user` scope only

```yaml
services:
  identity:
    url: identity:3000
    user:
      routes:
        - path: /api/users/{user_id}/api-keys
          methods: [GET, POST]
        - path: /api/users/{user_id}/api-keys/{key_id}
          methods: [DELETE]
        - path: /api/organizations
          methods: [POST, GET]
        - path: /api/organizations/{organization_id}
          methods: [GET, PATCH, DELETE]
        - path: /api/organizations/{organization_id}/members
          methods: [GET, POST]
        - path: /api/organizations/{organization_id}/members/{member_id}
          methods: [GET, PATCH, DELETE]
        - path: /api/organizations/{organization_id}/members/{member_id}/api-keys
          methods: [GET, POST]
        - path: /api/organizations/{organization_id}/members/{member_id}/api-keys/{key_id}
          methods: [DELETE]
        - path: /api/organizations/{organization_id}/invites
          methods: [GET, POST]
        - path: /api/organizations/{organization_id}/invites/{invite_id}
          methods: [DELETE]
        - path: /api/invites/redeem
          methods: [POST]
        - path: /api/organizations/{organization_id}/service-accounts
          methods: [GET, POST]
        - path: /api/organizations/{organization_id}/service-accounts/{service_account_id}
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
          required_scopes: [projects:write]
          rate_limit:
            requests: 20
            window_seconds: 60
```

## Validation Rules

Registry validates **before etcd**. Gateway validates again at load (expanded list form):

- `path` not empty; must be `/` or start with `/`
- `methods` not empty (list or non-empty map)
- Nested `methods` map keys: `GET`, `POST`, `PUT`, `PATCH`, `DELETE`, `HEAD`, `OPTIONS`
- Do not mix methods list and map on the same route (`422`)
- Nested maps expand at apply; duplicate `path`+method after expand → `422`
- At least one scope (`public`, `user`, and/or `organization`)
- `organization` scope requires `organization_param`; every expanded org path includes `{param}`
- `route_prefix` join rules at registry; etcd stores full paths only
- `required_scopes` if present: non-empty, `[a-z0-9:._-]+`, max 64 chars, max 32, unique
- `rate_limit` if object: `requests` > 0, `window_seconds` > 0. `true` is invalid
- Duplicate service names in merged registry: first wins, warning
- Malformed JSON values skipped with warning; other routes continue

The two copies of `route_config.rs` (gateway and route-registry) stay aligned.

## Platform Integrity

Auth delegation: [`gateway-contract.md`](gateway-contract.md). Layer split: [`identity-boundary.md`](identity-boundary.md). Admin write path: [`route-registry.md`](route-registry.md).
