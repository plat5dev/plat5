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
- **Apply:** `POST /apply` with a `routes.yml` body (JSON or YAML) — **upsert** of the services in the file. Services not in the file are left alone.
- **Identity:** public identity routes are operator-owned. Catalog: [`services/identity/routes.yml`](../services/identity/routes.yml). Apply it (or a subset). Dev compose may seed missing services from that file on first boot; it does not overwrite. Prod does not seed.

```bash
curl -sS -X POST http://localhost:5002/apply \
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
| `ADMIN_TOKEN` | Bearer token for admin API | Required |
| `SEED_ROUTES_DIR` | Optional; upsert **missing** services from YAML (dev) | empty |
| `PORT` | Admin API port | `5002` |
| `INTERNAL_PORT` | Health port | `5003` |

## Config Format (`routes.yml`)

Auth is **scopes** (`public`, `user`, `organization`, `member`).

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
        - path: /widgets
          methods: [GET, POST]
          required_scopes: [widgets:read]

        - path: /widgets/{id}
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
| `rate_limits` | `map<string, RateLimitPolicy>?` | Optional on the **service**. Named policies this service’s routes may reference. |
| `public` | `ScopeConfig?` | No authentication. |
| `user` | `ScopeConfig?` | User JWT or **user** API key. |
| `organization` | `ScopeConfig?` | **Member** API key or **member** session. Subject field: `organization_id`. |
| `member` | `ScopeConfig?` | Same credential as `organization`. Subject fields: `organization_id`, `member_id`. |
| `route_prefix` | `string?` | Optional on any scope. Registry expands into each `path` before etcd. Not applied to `upstream`. |
| `routes` | `array<RouteConfig>` | HTTP routes for this scope. |
| `path` | `string` | Match path (`/` or starts with `/`). Params are resource ids, not the subject. |
| `upstream` | `string?` | Absolute path template. Omitted means proxy `path` unchanged. Placeholders stay in etcd; the gateway substitutes at request time. Route-level only — not per-method. |
| `methods` | `array<string>` \| `map<string, MethodConfig>` | List form: allowed HTTP methods. Map form: per-verb config (see below). Do not mix list and map on the same route (`422`). |
| `required_scopes` | `string[]?` | Optional. Omitted = any admitted principal (including restricted keys). If set, a **restricted** API key (`scopes` non-null, including `[]`) must share at least one label. JWTs, unrestricted keys, and member sessions (`scopes: null`) skip. Validated at apply. Route-level value applies only to the flat methods list. |
| `rate_limit` | `false` \| `{requests, window_seconds}` \| `string` \| omitted | Omitted **inherits** the gateway fallback. `false` opts out (unlimited). Object = this route+method only. String = named policy on **this** service. Limiter subject follows route scope (`public`→ip, `user`→`user_id`, `organization`→`organization_id`, `member`→`member_id`). Route-level value applies only to the flat methods list. |

A service must define at least one scope. Multiple scopes may be present.

### `methods`

Two forms. Do not mix them on the same route (`422`).

**Flat list.** Optional route-level `required_scopes` / `rate_limit` apply to every method in the list.

```yaml
- path: /features
  methods: [GET, POST]
  required_scopes: [org:read]
  rate_limit: { requests: 60, window_seconds: 60 }
```

**Nested map.** Each key is an uppercase HTTP verb (`GET`, `POST`, `PUT`, `PATCH`, `DELETE`, `HEAD`, `OPTIONS`). The body may set `required_scopes` and/or `rate_limit` for that verb only. An empty body (`GET:` or `GET: {}`) means that method with no extra constraints. An empty methods map is rejected.

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

Nested maps are an **apply-time YAML convenience**. Registry `prepare_for_registry` / prefix expand turns each verb into its own `RouteConfig` row (same `path`, `methods: [THAT_VERB]`, `required_scopes` / `rate_limit` taken from that method entry). `upstream` stays on the path. After expand, etcd `methods` is always a string array. Duplicate `path`+method after expand → `422`.

Labels are opaque. `org:write` does not imply `org:read`.

### `required_scopes`

Labels follow the same hygiene as key mint: `[a-z0-9:._-]+`, max 64 chars, max 32, unique, non-empty list if present.

After match + admission: if the route has `required_scopes` **and** the credential is an API key with a non-null scopes list, the two lists must have a nonempty intersection or the gateway returns **403** `FORBIDDEN`. JWT, unrestricted keys, and member sessions (`scopes: null`) skip. `scopes: []` is restricted and cannot intersect — **403** on these routes, still admitted on unlabeled routes.

### `rate_limit`

Applies to **all admitted** routes (JWT, API key, and member session). Counters live in **Valkey** — replicas share one budget. `VALKEY_URL` is required to boot. Valkey error or timeout on a limited request → **503** `SERVICE_UNAVAILABLE`. The gateway opens a new connection when Valkey answers again.

Fixed window: the first increment opens the window; key TTL is `window_seconds`. Restarting Valkey resets open windows.

Route field (`rate_limit` on a route or nested method):

| YAML | Effect |
|------|--------|
| omitted | Inherit `RATE_LIMIT_REQUESTS` / `RATE_LIMIT_WINDOW_SECONDS`. `0` requests = unlimited fallback. |
| `false` | Unlimited for this route / verb |
| `{requests, window_seconds}` | Unique bucket for **this route+method only**. Both must be > 0. |
| policy name (string) | Named policy defined on **this** service. `true` is invalid. |

A name on a flat methods list puts every verb in that list on the named bucket. An inline object on a flat list still uses **per-method** buckets (`GET /features` ≠ `POST /features`) with the same numbers.

Named policies stay **names** in etcd (`rate_limit: writes` plus the service `rate_limits` map). Apply does not substitute `{requests, window_seconds}`. The gateway resolves the bucket at request time.

#### Named policies (`rate_limits`)

Optional map on the service. Routes may only reference a name defined **on that service**. Unknown name → **422**.

```yaml
services:
  projects:
    url: projects:3000
    rate_limits:
      writes:
        requests: 30
        window_seconds: 60
      org-writes:
        requests: 30
        window_seconds: 60
        shared: true
    organization:
      routes:
        - path: /projects
          methods: [POST]
          rate_limit: writes
        - path: /projects/{project_id}
          methods: [DELETE]
          rate_limit: org-writes
```

| Field | Rule |
|-------|------|
| map key | Policy name. `[a-z0-9:._-]+`, max 64. Unique in the service. |
| `requests` / `window_seconds` | Required. Both > 0. |
| `shared` | Optional bool. Omit / `false`: this service only. `true`: same bucket across services that declare the name. |

**Buckets** (subject appended):

| Policy | Bucket |
|--------|--------|
| omitted inherit / inline object | `{METHOD} {path} {subject}` |
| name, not shared | `{service}:{name}:{subject}` |
| name, `shared: true` | `{name}:{subject}` |

Limiter subject follows route scope and is not configurable: `public`→ip, `user`→`user_id`, `organization`→`organization_id`, `member`→`member_id`.

**`shared: true`** — opt-in cross-service join. Every service that uses the name declares the same table entry (`requests`, `window_seconds`, `shared: true`). Apply and `PUT /services/{name}` validate against **all** current services in Postgres, not only the payload.

- Same shared name, different numbers → **422**
- Same name, one `shared: true` and one local → **422**
- Local `writes` on two services → independent buckets; numbers may differ
- Inline object is its own bucket (`{METHOD} {path} {subject}`)
- One service claiming a shared name with no other consumer is fine

Exceed → **429** `RATE_LIMITED`, `Retry-After`, `details.retry_after_seconds`. Admitted limited routes also set `X-RateLimit-Limit` / `Remaining` / `Reset` (success and 429). See [`api-errors.md`](api-errors.md) and [`gateway-contract.md`](gateway-contract.md).

A separate failed-auth IP limiter (`RATE_LIMIT_AUTH_FAILURE_*`) covers unadmitted 401s and unmatched 404s. It is not per-route.

### `upstream`

`path` is the match. Its params are resource ids. `upstream` is an absolute template. Omitted means proxy `path` unchanged: the upstream has no subject id. A route that needs the subject sets `upstream`.

```yaml
user:
  routes:
    - path: /user/foos/{foo_id}
      upstream: /users/{subject.user_id}/foos/{path.foo_id}
      methods: [GET]

organization:
  routes:
    - path: /org/projects/{project_id}
      upstream: /organizations/{subject.organization_id}/projects/{path.project_id}
      methods: [GET, POST]

member:
  routes:
    - path: /member/api-keys
      upstream: /members/{subject.member_id}/api-keys
      methods: [GET, POST]
```

`route_prefix` expands `path` only, at apply, before etcd. `upstream` is not prefixed. etcd stores the full `path` and the `upstream` template with placeholders still in it. The gateway substitutes at request time. The registry does not.

Apply-time **422**:

- `subject.*` is only `user_id`, `organization_id`, `member_id`, and only a field that scope has.
- `path.*` names a param of the expanded `path`.
- A bare `{foo_id}` in `upstream` is rejected. The namespace is the point.
- `path` must not contain a param whose name is a subject field of that scope. `user` forbids `{user_id}`. `organization` forbids `{organization_id}`. `member` forbids `{organization_id}` and `{member_id}`.
- `upstream` must be omitted or an absolute path (starts with `/`, no `?` or `#`).

`{organization_id}` on a `user` route is a resource id. It is not admission input.

Substituted values are one path segment. Reject `/`, `?`, `#`. A bad path param is **400**. A bad subject id is **500**. Query string is preserved.

Unknown fields on the route schema are **422**.

## Scopes

| Scope | Auth | Subject fields |
|-------|------|----------------|
| `public` | No | none |
| `user` | User JWT or user API key | `user_id` |
| `organization` | Member API key or member session | `organization_id` |
| `member` | Same credential as `organization` | `organization_id`, `member_id` |

Subject fill and service rules: [`gateway-contract.md`](gateway-contract.md). Layer boundary and admission errors: [`identity-boundary.md`](identity-boundary.md).

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

## `organization` and `member`

Business APIs that should not own membership storage. The credential is a member of the org. The client does not name the subject.

`organization` fills `organization_id`. `member` fills that and `member_id`. Admission steps and errors: [`identity-boundary.md`](identity-boundary.md).

User-subject identity routes stay on `user` scope. Identity routes whose subject is the org or the member are published on those scopes. The catalog is [`services/identity/routes.yml`](../services/identity/routes.yml).

### `route_prefix` (optional, any scope)

Joined with each route `path` so configs stay short.

**Join rule:** path must be `/` (exactly the prefix) or start with `/`. Empty `path` invalid. When path is `/`, full path is the prefix with trailing slashes stripped.

**Expand site (locked):** Registry expands `route_prefix` + `path` **and nested `methods` maps** **before** writing etcd. Registry stores **full paths only** and **list-form `methods` only** — one expand site, no gateway/registry drift.

`upstream` is an absolute path template (not relative to `route_prefix`).

### Examples

#### identity catalog

Apply [`services/identity/routes.yml`](../services/identity/routes.yml) or a subset. Edge paths are `/user`, `/org`, and `/member`. `upstream` fills the identity URL. `GET /organizations` is not in the catalog.

#### Business service

```yaml
services:
  projects:
    url: projects:3000
    organization:
      route_prefix: /api
      routes:
        - path: /projects
          upstream: /organizations/{subject.organization_id}/projects
          methods: [GET, POST]
        - path: /projects/{project_id}
          upstream: /organizations/{subject.organization_id}/projects/{path.project_id}
          methods: [GET, PATCH, DELETE]
          required_scopes: [projects:write]
          rate_limit:
            requests: 20
            window_seconds: 60
```

The handler reads `organization_id` from the rewritten path. The client path has no subject id.

## Validation Rules

Registry validates **before etcd**. Gateway validates again at load (expanded list form):

- `path` not empty; must be `/` or start with `/`
- `methods` not empty (list or non-empty map)
- Nested `methods` map keys: `GET`, `POST`, `PUT`, `PATCH`, `DELETE`, `HEAD`, `OPTIONS`
- Do not mix methods list and map on the same route (`422`)
- Nested maps expand at apply; duplicate `path`+method after expand → `422`
- At least one scope (`public`, `user`, `organization`, and/or `member`)
- Unknown fields **422**
- `upstream` if present: absolute path; `subject.*` only a field that scope has; `path.*` names a param of the expanded path; bare `{foo}` rejected
- Expanded `path` must not contain a subject-field param of that scope
- `route_prefix` join rules at registry; etcd stores full paths only; `upstream` is not prefixed
- `required_scopes` if present: non-empty, `[a-z0-9:._-]+`, max 64 chars, max 32, unique
- `rate_limit` if object: `requests` > 0, `window_seconds` > 0. `true` is invalid
- `rate_limit` if string: names a policy on this service; policy name `[a-z0-9:._-]+`, max 64
- `rate_limits` keys: same hygiene, unique; values `requests` > 0, `window_seconds` > 0
- Shared policy names: same name across services must agree on `requests`, `window_seconds`, and `shared` (full desired state, apply and PUT)
- Duplicate service names in merged registry: first wins, warning
- Malformed JSON values skipped with warning; other routes continue

The two copies of `route_config.rs` (gateway and route-registry) stay aligned.

## Platform Integrity

Auth delegation: [`gateway-contract.md`](gateway-contract.md). Layer split: [`identity-boundary.md`](identity-boundary.md). Admin write path: [`route-registry.md`](route-registry.md).
