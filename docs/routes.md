# Route Publishing

Publish routes for the Plat5 gateway to discover and proxy traffic.

## Pipeline

```
Service (routes.yml) → control plane (Postgres desired state + revision)
                    → GET /internal/gateway (long-poll) → Gateway
```

Postgres is the source of truth. The gateway long-polls the control plane and swaps its whole route map on each change. Apply while gateways are down; they load the current map when they start.

Write path and feed details: [`control-plane.md`](control-plane.md).

## Control plane

Long-running service. Validates config, expands `route_prefix` and nested `methods` maps, records a revision in Postgres, and serves the current services to the gateway.

- **Local admin URL:** `http://localhost:5402`
- **Auth:** `Authorization: Bearer <ADMIN_TOKEN>`
- **Apply:** `POST /apply` with a `routes.yml` body (JSON or YAML) — **upsert** of the services in the file. Services not in the file are left alone.
- **Identity:** public identity routes are operator-owned. Catalog: [`services/identity/routes.yml`](../services/identity/routes.yml). Apply it (or a subset). Dev compose may seed missing services from that file on first boot; it does not overwrite. Prod does not seed.

```bash
curl -sS -X POST http://localhost:5402/apply \
  -H "Authorization: Bearer dev-admin-token" \
  -H "Content-Type: application/yaml" \
  --data-binary @routes.yml
```

Validation is at **apply time**, and the same checks run on `PUT /services/{name}` and on revision restore (a restored revision must pass today's rules). Malformed config → `422 VALIDATION_ERROR`; nothing written.

- Service `url` is required and must be exactly `http://host:port` (explicit port, no path or query), e.g. `http://my-service:3000`. `https://` → `422` saying TLS (https) upstreams aren't supported yet. Bare `host:port`, any other scheme, a missing port, or a path/query → `422 VALIDATION_ERROR` naming that form. Routes already stored with a bare `host:port` keep routing (the gateway strips the scheme) until re-applied. `http://user@host:port` → `422` saying credentials aren't allowed; a port outside 1–65535 → `422` saying it is out of range. A missing `url` → `422` telling you to set `url` (the CLI catches this first and points at `upstreams:` in `plat5.yml`).
- A service name is 1–128 characters of letters, digits, `.`, `_`, and `-`. It is a rate-limit bucket prefix, and the `service` on every audit event ([`audit.md`](audit.md)). `billing api` or `a/b` → `422`.
- Paths are canonical and are **rejected, not rewritten**: no trailing `/` (except `/` itself) and no uppercase letters outside `{params}` (param names keep their case). `/user/profile/` and `/USER/profile` → `422` naming the canonical form (`/user/profile`). This applies to the full path after `route_prefix` is joined.
- A path is what an audit event records as `route`, and what a request can match: visible ASCII (no spaces; percent-encode anything else), at most 1024 bytes. Param names are letters, digits, and `_`, not starting with a digit, at most 64 characters, at most 32 per path. `{project-id}` → `422`; use `{project_id}`.
- A route may belong to only one service, compared by **shape**: each `{param}` matches any other `{param}`, a literal segment matches only the same literal (a literal never matches a param). If an incoming method + shape is already owned by a **different** service, the whole apply is rejected with `409 ROUTE_CONFLICT`; the message lists each conflicting route and its current owner (and the owner's path when it differs, e.g. `GET /a/{x} (owned by 'a2' as /a/{y})`). Nothing is written.
  - Only the **same method** conflicts: `GET /a/{x}` in one service and `POST /a/{y}` in another are fine.
  - Literal vs param never conflicts: `/a/special` and `/a/{x}` can live in different services.
  - Inside one service, shapes don't conflict: `POST /subscribers/{id}/subscriptions/change` and `GET /subscribers/{id}/subscriptions/{subscription_id}` apply cleanly. Re-applying a service's own routes replaces them and is allowed.

After validation, all services in the batch commit in **one Postgres transaction** (each service gets a new revision). `200` means desired state is recorded; gateways see it on their next poll, within about a second.

Stored as JSON (not YAML): the control plane validates and canonicalizes at write time; the gateway deserializes into route types. Nested `methods` maps are expanded at apply so stored `methods` is always a string array.

Environment: [`control-plane.md`](control-plane.md#environment).
| `PORT` | Admin API port | `5002` |
| `INTERNAL_PORT` | Health port | `5003` |

## Config Format (`routes.yml`)

Auth is **scopes** (`public`, `user`, `organization`, `member`).

```yaml
services:
  my-service:
    url: http://my-service:3000
    public:
      routes:
        - path: /public/health
          methods: [GET]

    user:
      routes:
        - path: /widgets
          methods: [GET, POST]
          required_labels: [widgets:read]

        - path: /widgets/{id}
          methods: [GET, DELETE]
          rate_limit:
            requests: 30
            window_seconds: 60
```

Same path, different per-verb `required_labels` / `rate_limit` — nested `methods` map (expanded at apply into one stored row per verb):

```yaml
        - path: /features
          methods:
            GET:
              required_labels: [org:read]
            POST:
              required_labels: [org:write]
              rate_limit:
                requests: 100
                window_seconds: 1
```

`url` is exactly `http://host:port`, where `host` is anything the **gateway** can reach (cluster DNS name, `host.docker.internal`, an IP). No `https://` (TLS upstreams aren't supported yet), no path, no query; anything else is `422`. How you run the process is out of scope for Plat5.

### Fields

| Field | Type | Description |
|-------|------|-------------|
| `services` | `map<string, ServiceConfig>` | Top-level wrapper. Keys are service names. |
| `url` | `string` | Upstream, exactly `http://host:port` (e.g. `http://my-service:3000`). |
| `rate_limits` | `map<string, RateLimitPolicy>?` | Optional on the **service**. Named policies this service’s routes may reference. |
| `timeouts` | `{connect_ms?, read_ms?}?` | Optional on the **service**. Upstream timeouts in ms, each 1–600000. One left out uses the gateway's `UPSTREAM_CONNECT_TIMEOUT_MS` / `UPSTREAM_READ_TIMEOUT_MS`. |
| `public` | `ScopeConfig?` | No authentication. |
| `user` | `ScopeConfig?` | A person's credential: user JWT or **user** API key. Subject field: `user_id`. |
| `organization` | `ScopeConfig?` | A member's credential: **member** API key or **session**. Subject field: `organization_id`. |
| `member` | `ScopeConfig?` | Same credential as `organization`. Subject fields: `organization_id`, `member_id`. |
| `route_prefix` | `string?` | Optional on any scope. Expanded into each `path` at write time. Not applied to `upstream`. |
| `routes` | `array<RouteConfig>` | HTTP routes for this scope. |
| `path` | `string` | Match path (`/` or starts with `/`). Params are resource ids, not the subject. |
| `upstream` | `string?` | Absolute path template. Omitted means proxy `path` unchanged. Placeholders stay in the stored config; the gateway substitutes at request time. Route-level only — not per-method. |
| `methods` | `array<string>` \| `map<string, MethodConfig>` | List form: allowed HTTP methods. Map form: per-verb config (see below). Do not mix list and map on the same route (`422`). |
| `required_labels` | `string[]?` | Optional. Omitted = any admitted principal. If set, a member credential must hold `*` or share at least one label: any one of the list, not all. User credentials skip. Cannot include `*`. Validated at apply. Route-level value applies only to the flat methods list. |
| `rate_limit` | `false` \| `{requests, window_seconds}` \| `string` \| omitted | Omitted **inherits** the gateway fallback. `false` opts out (unlimited). Object = this route+method only. String = named policy on **this** service. Limiter subject follows route scope (`public`→ip, `user`→`user_id`, `organization`→`organization_id`, `member`→`member_id`). Route-level value applies only to the flat methods list. |
| `audit` | `bool?` | `user`, `organization`, and `member` routes. Omitted = method default: `POST` / `PUT` / `PATCH` / `DELETE` audited, `GET` / `HEAD` not. `true` / `false` overrides. Route-level value applies only to the flat methods list. See [`audit`](#audit). |

A service must define at least one scope. Multiple scopes may be present.

### `methods`

Two forms. Do not mix them on the same route (`422`).

**Flat list.** Optional route-level `required_labels` / `rate_limit` / `audit` apply to every method in the list.

```yaml
- path: /features
  methods: [GET, POST]
  required_labels: [org:read]
  rate_limit: { requests: 60, window_seconds: 60 }
```

**Nested map.** Each key is an uppercase HTTP verb (`GET`, `POST`, `PUT`, `PATCH`, `DELETE`, `HEAD`, `OPTIONS`). The body may set `required_labels`, `rate_limit`, and/or `audit` for that verb only. An empty body (`GET:` or `GET: {}`) means that method with no extra constraints. An empty methods map is rejected.

```yaml
- path: /features
  methods:
    GET:
      required_labels: [org:read]
    POST:
      required_labels: [org:write]
      rate_limit:
        requests: 100
        window_seconds: 1
```

Nested maps are an **apply-time YAML convenience**. The control plane's `prepare_for_registry` / prefix expand turns each verb into its own `RouteConfig` row (same `path`, `methods: [THAT_VERB]`, `required_labels` / `rate_limit` / `audit` taken from that method entry). `upstream` stays on the path. After expand, stored `methods` is always a string array. Duplicate `path`+method after expand → `422`.

Labels are opaque. `org:write` does not imply `org:read`.

### `required_labels`

Labels follow the same hygiene as roles: `[a-z0-9:._-]+`, max 64 chars, max 32, unique, non-empty list if present.

After admission and the rate limit: if the route has `required_labels`, a member credential's `labels` must hold `*` or share a label with the route, or the gateway returns **403** `FORBIDDEN`. `[a, b]` means a or b. `labels: ["*"]` (a `["*"]` role, or roles off) passes. `labels: []` cannot intersect — **403** on these routes, still admitted on unlabeled routes.

A credential carries its principal's labels. For a member key or session, validate's list is the member's role labels. Keys and sessions cannot narrow it. A role is how a deployment gives members labels: [`roles.md`](roles.md). User credentials have no role and skip, so a label on a `user` route constrains nothing. Put labels on `organization` and `member` routes. The 403 `details.required_labels` echoes the route's list ([`gateway-contract.md`](gateway-contract.md)).

### `rate_limit`

Applies to **all admitted** routes (JWT, API key, and session). Counters live in **Valkey** — replicas share one budget. `VALKEY_URL` is required to boot. Valkey error or timeout on a limited request → **503** `SERVICE_UNAVAILABLE`. The gateway opens a new connection when Valkey answers again.

Fixed window: the first increment opens the window; key TTL is `window_seconds`. Restarting Valkey resets open windows.

Route field (`rate_limit` on a route or nested method):

| YAML | Effect |
|------|--------|
| omitted | Inherit `RATE_LIMIT_REQUESTS` / `RATE_LIMIT_WINDOW_SECONDS`. `0` requests = unlimited fallback. |
| `false` | Unlimited for this route / verb |
| `{requests, window_seconds}` | Unique bucket for **this route+method only**. Both must be > 0. |
| policy name (string) | Named policy defined on **this** service. `true` is invalid. |

A name on a flat methods list puts every verb in that list on the named bucket. An inline object on a flat list still uses **per-method** buckets (`GET /features` ≠ `POST /features`) with the same numbers.

Named policies stay **names** in the stored config (`rate_limit: writes` plus the service `rate_limits` map). Apply does not substitute `{requests, window_seconds}`. The gateway resolves the bucket at request time.

#### Named policies (`rate_limits`)

Optional map on the service. Routes may only reference a name defined **on that service**. Unknown name → **422**.

```yaml
services:
  projects:
    url: http://projects:3000
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

Limiter subject follows route scope and is not configurable: `public`→ip, `user`→`user_id`, `organization`→`organization_id`, `member`→`member_id`. Every `organization` route shares one bucket per org, so a runaway service account can throttle the people in its org. Give a heavy caller's routes their own named policy.

**`shared: true`** — opt-in cross-service join. Every service that uses the name declares the same table entry (`requests`, `window_seconds`, `shared: true`). Apply and `PUT /services/{name}` validate against **all** current services in Postgres, not only the payload.

- Same shared name, different numbers → **422**
- Same name, one `shared: true` and one local → **422**
- Local `writes` on two services → independent buckets; numbers may differ
- Inline object is its own bucket (`{METHOD} {path} {subject}`)
- One service claiming a shared name with no other consumer is fine

Exceed → **429** `RATE_LIMITED`, `Retry-After`, `details.retry_after_seconds`. Admitted limited routes also set `X-RateLimit-Limit` / `Remaining` / `Reset` (success and 429). See [`api-errors.md`](api-errors.md) and [`gateway-contract.md`](gateway-contract.md).

A separate failed-auth IP limiter (`RATE_LIMIT_AUTH_FAILURE_*`) covers unadmitted 401s and unmatched 404s. It is not per-route.

The limiter runs before `required_labels`. A label-denied **403** counts against the route's limit.

### `audit`

Whether the gateway records this route+method in the audit log of the realm that owns what it changes: the org's, or on a `user` route the person's. Model, delivery, and the event: [`audit.md`](audit.md).

| YAML | Effect |
|------|--------|
| omitted | Method default: `POST`, `PUT`, `PATCH`, `DELETE` audited. `GET`, `HEAD` not |
| `true` | Audited. Use it on a read worth recording, e.g. one that returns secrets |
| `false` | Not audited. Use it on a high-volume write nobody needs in the log |

`user`, `organization`, and `member` routes. On `public` → **422**: that scope has no realm to log into. `OPTIONS` is answered as a preflight and never audited.

Omitted stays omitted in the stored config. The gateway applies the method default. With audit off for the deployment, the gateway ignores the field.

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

`route_prefix` expands `path` only, at apply, before storing. `upstream` is not prefixed. The control plane stores the full `path` and the `upstream` template with placeholders still in it. The gateway substitutes at request time. The control plane does not.

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
| `organization` | Member API key or session | `organization_id` |
| `member` | Same credential as `organization` | `organization_id`, `member_id` |

Subject fill and service rules: [`gateway-contract.md`](gateway-contract.md). Layer boundary and admission errors: [`identity-boundary.md`](identity-boundary.md).

## Gateway Behavior

### Startup

1. `GET {CONTROL_PLANE_URL}/internal/gateway`.
2. Validate, build `RouteMap` and the CORS policy.
3. Retry while the control plane is down. The gateway does not listen until it has a snapshot: without one, every path would be **404**.

### Runtime Updates

1. Long-poll `GET /internal/gateway?after={revision}`.
2. On a newer revision: **full reload** of routes and CORS, swapped whole (small set; simpler than incremental).
3. Reload failure → keep current routes, log an error, and move past that revision.
4. Control plane down → keep current routes, retry.

### Service Health vs Route Existence

Decoupled. Service down → gateway still knows the route → **502** `SERVICE_UNAVAILABLE`. Missing route (nothing registered that path) → **404**.

## `organization` and `member`

Business APIs that should not own membership storage. The credential is a member of the org. The client does not name the subject.

`organization` fills `organization_id`. `member` fills that and `member_id`. Admission steps and errors: [`identity-boundary.md`](identity-boundary.md).

User-subject identity routes stay on `user` scope. Identity routes whose subject is the org or the member are published on those scopes. The catalog is [`services/identity/routes.yml`](../services/identity/routes.yml).

### `route_prefix` (optional, any scope)

Joined with each route `path` so configs stay short.

**Join rule:** path must be `/` (exactly the prefix) or start with `/`. Empty `path` invalid. When path is `/`, full path is the prefix with trailing slashes stripped.

**Expand site (locked):** The control plane expands `route_prefix` + `path` **and nested `methods` maps** **before** storing. It stores **full paths only** and **list-form `methods` only** — one expand site, no gateway/control-plane drift.

`upstream` is an absolute path template (not relative to `route_prefix`).

### Examples

#### identity catalog

Apply [`services/identity/routes.yml`](../services/identity/routes.yml) or a subset. Edge paths are `/user`, `/org`, and `/member`. `upstream` fills the identity URL. `GET /organizations`, `GET /users`, `PATCH /users/{user_id}/status`, and `GET /members/{member_id}/user` are not in the catalog: they are for staff.

#### Business service

```yaml
services:
  projects:
    url: http://projects:3000
    organization:
      route_prefix: /api
      routes:
        - path: /projects
          upstream: /organizations/{subject.organization_id}/projects
          methods: [GET, POST]
        - path: /projects/{project_id}
          upstream: /organizations/{subject.organization_id}/projects/{path.project_id}
          methods: [GET, PATCH, DELETE]
          required_labels: [projects:write]
          rate_limit:
            requests: 20
            window_seconds: 60
```

The handler reads `organization_id` from the rewritten path. The client path has no subject id.

## Validation Rules

The control plane validates **before storing**. The gateway validates again at load (expanded list form):

- Service name: 1–128 of `[A-Za-z0-9._-]` (control plane)
- `path` not empty; must be `/` or start with `/`
- Expanded `path`: visible ASCII, at most 1024 bytes; at most 32 params, each `[A-Za-z_][A-Za-z0-9_]*`, at most 64 characters (control plane)
- `methods` not empty (list or non-empty map)
- Nested `methods` map keys: `GET`, `POST`, `PUT`, `PATCH`, `DELETE`, `HEAD`, `OPTIONS`
- Do not mix methods list and map on the same route (`422`)
- Nested maps expand at apply; duplicate `path`+method after expand → `422`
- At least one scope (`public`, `user`, `organization`, and/or `member`)
- Unknown fields **422**
- `upstream` if present: absolute path; `subject.*` only a field that scope has; `path.*` names a param of the expanded path; bare `{foo}` rejected
- Expanded `path` must not contain a subject-field param of that scope
- `route_prefix` join rules at the control plane; it stores full paths only; `upstream` is not prefixed
- `required_labels` if present: non-empty, `[a-z0-9:._-]+`, max 64 chars, max 32, unique
- `rate_limit` if object: `requests` > 0, `window_seconds` > 0. `true` is invalid
- `rate_limit` if string: names a policy on this service; policy name `[a-z0-9:._-]+`, max 64
- `rate_limits` keys: same hygiene, unique; values `requests` > 0, `window_seconds` > 0
- `audit` if present: a boolean, on a `user`, `organization`, or `member` route or method only
- `timeouts` if present: `connect_ms` and `read_ms` each 1–600000; unknown fields **422**
- Shared policy names: same name across services must agree on `requests`, `window_seconds`, and `shared` (full desired state, apply and PUT)
- A snapshot that fails the gateway's validation is not loaded; the gateway keeps its current routes

The two copies of `route_config.rs` (gateway and control-plane) stay aligned.

## Platform Integrity

Auth delegation: [`gateway-contract.md`](gateway-contract.md). Layer split: [`identity-boundary.md`](identity-boundary.md). Admin write path: [`control-plane.md`](control-plane.md).
