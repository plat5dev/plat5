# Control plane

The deployment's desired state: routes, roles, and CORS origins. The operator writes it through an admin HTTP API. The control plane validates each write, records it with revisions in Postgres, and serves the current state to the gateway and identity. It is Plat5's only supported write path for any of these.

## What it holds

The control plane holds the operator's typed, deployment-wide state that changes while the deployment runs. Everything that belongs to a realm (people, orgs, members, keys) stays in identity.

| Document | Read by | Contract |
|----------|---------|----------|
| Services (routes, named rate limits, upstream timeouts) | gateway | [`routes.md`](routes.md) |
| `roles` | identity | [`roles.md`](roles.md) |
| `cors` | gateway | [below](#cors) |

Each document has its own schema and its own validation. There is no free-form key.

These stay out of it, as process env:

| Kept out | Why |
|----------|-----|
| Wiring: URLs, ports | Needed to reach the control plane in the first place |
| Secrets: `ADMIN_TOKEN`, `INTERNAL_AUTH_TOKEN` | Not desired state |
| Trust anchors: `AUTH_ISSUER`, `AUTH_JWKS_URI`, audiences | Changing the issuer lets you sign in as anyone; `ADMIN_TOKEN` must not be able to |
| Switches whose flip changes what stored data means: `ROLES_ENABLED`, `AUDIT_ENABLED` | Turning roles on leaves every member without a role. These are a migration, made at boot |
| `APIKEY_BRAND`, `APIKEY_CACHE_TTL_SECS`, gateway rate-limit fallback | Set at boot; changing them while running buys nothing |
| Per-org data, such as per-org roles (deferred) | Org data. Identity |

## Pipeline

```
routes.yml → POST /apply (or PUT /services/{name})  ┐
roles.yml  → PUT /roles                             ├→ Postgres schema `control_plane` (revision, head)
           → PUT /cors                              ┘
                                                       ├→ GET /internal/gateway  ← gateway (long-poll)
                                                       └→ GET /internal/roles    ← identity (long-poll)
```

Postgres is the source of truth, and the only store. Every write bumps one revision for the whole control plane (the **head**) in the same transaction as the change. Writes are serialized on it, so head order is commit order. Readers long-poll the head.

## Local URLs

| Surface | URL |
|---------|-----|
| Admin API (dev compose) | `http://localhost:5402` |
| Internal (feeds, health, metrics) | container `:5003` |

Prod compose does **not** publish the admin port by default.

## Auth

Admin API routes require:

```
Authorization: Bearer <ADMIN_TOKEN>
```

Local default: `dev-admin-token` (`ADMIN_TOKEN` env). Required and non-empty in all environments.

`/internal/*` on `INTERNAL_PORT` requires `X-Plat5-Internal-Token` when `INTERNAL_AUTH_TOKEN` is set, the same shared token the gateway sends identity and audit.

`ADMIN_TOKEN` can route traffic and change what every role grants. Treat it as the deployment's root credential. The revisions below are its history.

## API

### Services

| Method | Path | Body | Notes |
|--------|------|------|--------|
| `POST` | `/apply` | `routes.yml` shape (`services: {…}`) JSON or YAML | Validate, expand, one PG transaction |
| `GET` | `/services` | — | Current (non-deleted) services |
| `GET` | `/services/{name}` | — | Current config plus `name` / `revision` |
| `PUT` | `/services/{name}` | `ServiceConfig` JSON | New revision |
| `DELETE` | `/services/{name}` | — | Tombstone revision |
| `GET` | `/services/{name}/revisions` | — | History (includes delete revisions) |
| `GET` | `/services/{name}/revisions/{rev}` | — | One revision (`config` is `null` if delete) |
| `POST` | `/services/{name}/revisions/{rev}/restore` | — | New revision copying that config |

Apply is **upsert** of the services in the file. Services not in the file are left alone.

Named `rate_limits` live on each `ServiceConfig`. Apply and `PUT /services/{name}` validate `shared: true` policy names against **all** current services, not only the payload. Schema: [`routes.md`](routes.md).

Apply the identity catalog ([`services/identity/routes.yml`](../services/identity/routes.yml)) or a subset. Omitting a path hides it from the gateway. The identity process still serves that path on the network.

### Documents

`roles` and `cors` share one shape of API. `{kind}` is `roles` or `cors`.

| Method | Path | Body | Notes |
|--------|------|------|--------|
| `GET` | `/{kind}` | — | Current document plus `revision`. **404** if never written |
| `PUT` | `/{kind}` | The document, JSON or YAML | Validate, new revision |
| `GET` | `/{kind}/revisions` | — | History, newest first |
| `GET` | `/{kind}/revisions/{rev}` | — | One revision |
| `POST` | `/{kind}/revisions/{rev}/restore` | — | New revision copying that document, validated as if written now |

A document has no delete. `PUT` replaces the whole document. A body that fails validation is **422** `VALIDATION_ERROR` and writes nothing; an unknown key is **422**.

Revision responses:

```json
{ "kind": "roles", "revision": 2, "config": { … }, "request_id": "…", "created_at": "…" }
```

#### Roles

The body is the roles file: [`roles.md`](roles.md#the-roles-document).

```bash
curl -sS -X PUT http://localhost:5402/roles \
  -H "Authorization: Bearer dev-admin-token" \
  -H "Content-Type: application/yaml" \
  --data-binary @roles.yml
```

The control plane applies the same rules identity does. It cannot see members, so removing a role that members hold is allowed: those members fail closed ([`roles.md`](roles.md#resolution)).

#### CORS

```json
{ "allowed_origins": ["https://app.example.com", "http://localhost:5173"] }
```

| Field | Rule |
|-------|------|
| `allowed_origins` | Up to 100. Each is `https://host` or `http://host`, with an optional `:port`. Lowercase, no path, no trailing `/`, no wildcard, no repeats: the gateway compares it byte for byte with the browser's `Origin` |

An empty list, or no `cors` document, answers CORS with `*`. Otherwise the gateway echoes an origin in the list and sends no `Access-Control-Allow-Origin` for any other. Request-time behavior: [`gateway-contract.md`](gateway-contract.md#cors).

### Apply example

CLI:

```bash
plat5 routes apply ./routes.yml
plat5 roles apply ./roles.yml
```

curl:

```bash
curl -sS -X POST http://localhost:5402/apply \
  -H "Authorization: Bearer dev-admin-token" \
  -H "Content-Type: application/yaml" \
  --data-binary @routes.yml
```

```yaml
services:
  widgets:
    url: http://host.docker.internal:3000   # http://host:port the gateway can reach
    user:
      routes:
        - path: /widgets
          methods: [GET, POST]
```

`url` is from the **gateway’s** network view (in-cluster DNS, `host.docker.internal`, etc.). It must be exactly `http://host:port` (explicit port, no path or query; bare `host:port`, `https://` and other schemes are rejected). Plat5 does not run your app process.

### Response shape

Success apply (`200`) — desired state committed:

```json
{
  "results": [
    { "service": "widgets", "status": "upserted", "revision": 1 }
  ]
}
```

Validation / auth / empty body failures use the Plat5 envelope (`api-errors.md`) and write nothing. A bad or missing service `url` is `422 VALIDATION_ERROR`. A non-canonical path (trailing `/`, uppercase outside `{params}`) is `422`. So is a service name, path, or path param name that an audit event cannot carry ([routes.md](routes.md)). A method + path shape (params match params, literals match only literals) already owned by a **different** service rejects the whole apply with `409 ROUTE_CONFLICT` (message lists each route and its owner). `PUT /services/{name}` and restore run exactly the same checks; see [routes.md](routes.md). Re-applying a service's own routes replaces them and is not a conflict. A Postgres failure rolls the whole batch back (`503`). `200` means readers will see it on their next poll.

## Revisions

Each write (apply/put/delete/restore) appends a revision. Restore appends a new revision that copies an old config after validating it like an apply: an old revision with a now-invalid `url` or path is `422`, and one whose routes another service owns now is `409 ROUTE_CONFLICT`.

Delete stores `config: null`. History remains. Restore of a delete revision is `422`.

A service's or a document's `revision` counts its own writes. The head counts every write, and is what readers poll.

## Internal feeds

On `INTERNAL_PORT`. Each is one read, at one head, in one Postgres snapshot.

| Path | Reader | `200` body |
|------|--------|------------|
| `GET /internal/gateway` | gateway | `{ "revision", "services": { name → ServiceConfig }, "cors": { "allowed_origins": [] } }` |
| `GET /internal/roles` | identity | `{ "revision", "roles": { …the roles document } }`. **404** while there is no roles document |

`revision` is the head. `services` is in the stored form: full paths, list-form `methods` (`route_prefix` and nested `methods` maps already expanded). `cors` is the empty list when there is no document.

Without `after`, the answer is immediate. With `?after={revision}`, the control plane waits until the head is past `after`, for up to 25 seconds. Nothing newer in that time is **204**, and the reader asks again. A cursor ahead of the head (the store was reset) is answered at once.

Feed reads stay off traces and request metrics, like health checks: a long-poll is one 25-second span every cycle per reader, and its duration measures nothing. A 5xx is still logged. The readers don't trace their polls either.

Any write wakes every reader. A reader that gets a document it already has applies it again; that is cheap and keeps one cursor.

Each control-plane process polls the head in Postgres every second, so a write through any replica reaches readers of every replica within about a second.

### Readers

| | Gateway | Identity (roles on) |
|--|--|--|
| Boot | Waits for the first snapshot; retries while the control plane is down. Does not listen until it has one | Waits for the roles document; retries while the control plane is down or has none. Does not listen until it has one |
| Running | Long-polls; swaps routes and CORS whole on each change | Long-polls; swaps roles whole on each change |
| Control plane down | Keeps serving the last snapshot | Keeps the last roles |
| A snapshot it cannot load | Keeps the current one, logs, and moves its cursor past it | Same |

A reader never treats "nothing loaded" as an empty state: no routes would answer **404** for everything, and no roles would read as roles off, which grants every label.

## Environment

| Variable | Default | Purpose |
|----------|---------|---------|
| `PORT` | `5002` | Admin API |
| `INTERNAL_PORT` | `5003` | Feeds, health, metrics |
| `DATABASE_URL` | required | Postgres; schema `control_plane` |
| `ADMIN_TOKEN` | required | Bearer token |
| `INTERNAL_AUTH_TOKEN` | empty | Required on `/internal/*` when set |
| `SEED_ROUTES_DIR` | empty | Apply YAML for services with **no** history yet |
| `SEED_ROLES_FILE` | empty | Write this roles file when there has **never** been a roles document |

Ready is **503** unless Postgres answers.

A database from the route registry keeps its data: on first boot the control plane renames schema `routes` to `control_plane` and adds what is new.

## Seeds

Dev compose mounts `services/identity/routes.yml` and `services/audit/routes.yml` → `/seed/` and sets `SEED_ROUTES_DIR`. On boot, missing services (no `services` row) are applied. Existing rows — including deleted — are not overwritten.

Both composes mount [`compose/roles.yml`](../compose/roles.yml) and set `SEED_ROLES_FILE`, so a new deployment has roles and identity can start. Once a roles document exists, the file is never read again: change roles with `PUT /roles`.

Prod compose does **not** seed routes. Apply identity routes yourself (CLI or curl).

## Validation

Route types and validation live in each of gateway and control-plane (`src/route_config.rs`). Keep them aligned deliberately; the stored JSON and [`routes.md`](routes.md) are the contract. Expand (`route_prefix` and nested `methods` maps) happens at write time only.

Roles validation lives in each of control-plane (`src/documents.rs`) and identity (`roles/`). Identity checks again on receipt, so a document it cannot use never replaces the one it has.

## Related

- [`routes.md`](routes.md) — config format and scopes
- [`roles.md`](roles.md) — roles
- [`gateway-contract.md`](gateway-contract.md) — request-time behavior
