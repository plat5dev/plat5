# Route registry

Admin HTTP API that writes gateway routes to etcd. Plat5’s only supported write path for the route registry.

Gateway still **loads and watches** etcd; it does not expose route CRUD.

## Pipeline

```
routes.yml → POST /v1/apply (or PUT /v1/services/{name}) → etcd → gateway watch
```

Seed on boot: platform identity routes (`identity`) from `SEED_ROUTES_DIR`.

## Local URLs

| Surface | URL |
|---------|-----|
| Admin API (dev compose) | `http://localhost:5002` |
| Health (internal) | container `:5003` (`/health/live`, `/health/ready`) |

Prod compose does **not** publish the admin port by default.

## Auth

All `/v1/*` routes require an admin token:

```
Authorization: Bearer <ADMIN_TOKEN>
```

or

```
X-Plat5-Admin-Token: <ADMIN_TOKEN>
```

Local default: `dev-admin-token` (`ADMIN_TOKEN` env). Required and non-empty in all environments.

## API

| Method | Path | Body | Notes |
|--------|------|------|--------|
| `POST` | `/v1/apply` | `routes.yml` shape (`services: {…}`) JSON or YAML | Validate, expand `route_prefix`, upsert each service |
| `GET` | `/v1/services` | — | List all registered services |
| `GET` | `/v1/services/{name}` | — | Get one `ServiceConfig` (post-expand) |
| `PUT` | `/v1/services/{name}` | `ServiceConfig` JSON | Upsert one service |
| `DELETE` | `/v1/services/{name}` | — | Delete; platform services need `?force=true` |

### Apply example

CLI:

```bash
plat5 routes apply ./routes.yml
```

curl:

```bash
curl -sS -X POST http://localhost:5002/v1/apply \
  -H "Authorization: Bearer dev-admin-token" \
  -H "Content-Type: application/yaml" \
  --data-binary @routes.yml
```

```yaml
services:
  widgets:
    url: host.docker.internal:3000   # any URL the gateway can reach
    user:
      routes:
        - path: /api/widgets
          methods: [GET, POST]
```

`url` is from the **gateway’s** network view (in-cluster DNS, public HTTPS, `host.docker.internal`, etc.). Plat5 does not run your app process.

### Response shape

Success apply (`200`):

```json
{
  "results": [
    { "service": "widgets", "status": "upserted" }
  ]
}
```

Apply is **best-effort per service** after full pre-write validation. If a put fails mid-batch, earlier services stay written; the response is `503` with per-service status (not a pure error envelope):

```json
{
  "results": [
    { "service": "a", "status": "upserted" },
    { "service": "b", "status": "failed", "error": "Service temporarily unavailable" },
    { "service": "c", "status": "skipped" }
  ]
}
```

| `status` | Meaning |
|----------|---------|
| `upserted` | Written to etcd |
| `failed` | Put failed; `error` has the message |
| `skipped` | Not attempted (after a prior failure in the same apply) |

Validation / auth / empty body failures still use the Plat5 envelope (`api-errors.md`) and write nothing.

## etcd contract (unchanged)

| | |
|--|--|
| Key | `identity/gateway/routes/{service_name}` |
| Value | JSON `ServiceConfig` with **full paths** (`route_prefix` already expanded) |

## Environment

| Variable | Default | Purpose |
|----------|---------|---------|
| `PORT` | `5002` | Admin API |
| `INTERNAL_PORT` | `5003` | Health |
| `ETCD_URL` | `http://localhost:2379` | etcd |
| `ADMIN_TOKEN` | required | Bearer token |
| `SEED_ROUTES_DIR` | empty | Directory of `*.yml` / `*.yaml` to upsert on boot |
| `PLATFORM_SERVICES` | `identity` | Names protected from delete without `?force=true` |

## Platform seed

Compose mounts:

- `services/identity/routes.yml` → `/seed/identity.yml`

Registry upserts seed files on every start (idempotent).

## Validation

Route types and validation live in each of gateway and route-registry (`src/route_config.rs`). Keep them aligned deliberately; etcd JSON and [`routes.md`](routes.md) are the contract. Expand happens at write time only.

## Related

- [`routes.md`](routes.md) — config format and scopes
- [`gateway-contract.md`](gateway-contract.md) — request-time behavior
