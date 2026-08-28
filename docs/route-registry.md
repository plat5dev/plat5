# Route registry

Admin HTTP API: desired route state in Postgres, live projection in etcd. Plat5’s only supported write path for the route registry.

Gateway still **loads and watches** etcd; it does not talk to Postgres.

## Pipeline

```
routes.yml → POST /apply (or PUT /services/{name})
  → Postgres schema `routes` (revision)
  → etcd `edge/gateway/routes/{name}`
  → gateway watch
```

Postgres is the source of truth. etcd is a projection. A reconciler retries if a put/delete is missed.

## Local URLs

| Surface | URL |
|---------|-----|
| Admin API (dev compose) | `http://localhost:5002` |
| Health (internal) | container `:5003` (`/health/live`, `/health/ready`) |

Prod compose does **not** publish the admin port by default.

## Auth

Admin API routes require:

```
Authorization: Bearer <ADMIN_TOKEN>
```

Local default: `dev-admin-token` (`ADMIN_TOKEN` env). Required and non-empty in all environments.

## API

| Method | Path | Body | Notes |
|--------|------|------|--------|
| `POST` | `/apply` | `routes.yml` shape (`services: {…}`) JSON or YAML | Validate, expand, one PG transaction, project |
| `GET` | `/services` | — | Current (non-deleted) services |
| `GET` | `/services/{name}` | — | Current config plus `name` / `revision` |
| `PUT` | `/services/{name}` | `ServiceConfig` JSON | New revision + project |
| `DELETE` | `/services/{name}` | — | Tombstone revision; remove etcd key |
| `GET` | `/services/{name}/revisions` | — | History (includes delete revisions) |
| `GET` | `/services/{name}/revisions/{rev}` | — | One revision (`config` is `null` if delete) |
| `POST` | `/services/{name}/revisions/{rev}/restore` | — | New revision copying that config |

Apply is **upsert** of the services in the file. Services not in the file are left alone. There is no prune.

Identity public routes are not special. Apply the catalog ([`services/identity/routes.yml`](../services/identity/routes.yml)) or a subset. Omitting a path does not disable the identity process — it only hides those routes from the gateway.

### Apply example

CLI:

```bash
plat5 routes apply ./routes.yml
```

curl:

```bash
curl -sS -X POST http://localhost:5002/apply \
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

Success apply (`200`) — desired state committed:

```json
{
  "results": [
    { "service": "widgets", "status": "upserted", "revision": 1 }
  ]
}
```

Validation / auth / empty body failures use the Plat5 envelope (`api-errors.md`) and write nothing. A Postgres failure rolls the whole batch back (`503`). etcd projection is retried by the reconciler; apply does not fail after a successful commit.

## Revisions

Each write (apply/put/delete/restore) appends a revision. Rollback is a new revision that copies an old config, not a rewind.

Delete stores `config: null` and clears the etcd key. History remains. Restore of a delete revision is `422`.

## etcd projection

| | |
|--|--|
| Key | `edge/gateway/routes/{service_name}` |
| Value | JSON `ServiceConfig` with **full paths** and list-form `methods` (`route_prefix` and nested methods maps already expanded) |

## Environment

| Variable | Default | Purpose |
|----------|---------|---------|
| `PORT` | `5002` | Admin API |
| `INTERNAL_PORT` | `5003` | Health |
| `ETCD_URL` | `http://localhost:2379` | etcd (projection) |
| `DATABASE_URL` | required | Postgres; schema `routes` |
| `ADMIN_TOKEN` | required | Bearer token |
| `SEED_ROUTES_DIR` | empty | Dev: apply YAML for services with **no** history yet |

Ready is **503** unless Postgres and etcd both answer.

## Dev seed

Compose mounts `services/identity/routes.yml` → `/seed/identity.yml` and sets `SEED_ROUTES_DIR`. On boot, missing services (no `routes.services` row) are applied. Existing rows — including deleted — are not overwritten.

Prod compose does **not** seed. Apply identity routes yourself (CLI or curl).

## Validation

Route types and validation live in each of gateway and route-registry (`src/route_config.rs`). Keep them aligned deliberately; etcd JSON and [`routes.md`](routes.md) are the contract. Expand (`route_prefix` and nested `methods` maps) happens at write time only.

## Related

- [`routes.md`](routes.md) — config format and scopes
- [`gateway-contract.md`](gateway-contract.md) — request-time behavior
