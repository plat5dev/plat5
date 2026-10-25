# Control plane admin API

Concept: [config](../../concepts/config.md). Environment: [`reference/env.md`](../env.md#control-plane).

## Local URLs

| Surface | URL |
|---------|-----|
| Admin API (dev compose) | `http://localhost:5402` |
| Internal (feeds, health, metrics) | container `:5003` |

Prod compose does **not** publish the admin port by default.

## Auth

Admin API routes require `Authorization: Bearer <ADMIN_TOKEN>`. Local default: `dev-admin-token`. Required and non-empty in all environments.

`/internal/*` on `INTERNAL_PORT` requires `X-Plat5-Internal-Token` when `INTERNAL_AUTH_TOKEN` is set, the same shared token the gateway sends identity and audit.

`ADMIN_TOKEN` can route traffic, change what every role grants, and remove a kind with its nodes. Treat it as the deployment's root credential. The revisions below are its history.

## API

| Method | Path | Notes |
|--------|------|-------|
| `POST` | `/apply?dry_run=true` | The plan. Writes nothing |
| `POST` | `/apply` | Apply, with `expect` and `confirm`. JSON or YAML |
| `GET` | `/config` | The current config, as applied, and its `revision`. **404** before the first apply |
| `GET` | `/revisions` | History, newest first: `revision`, `created_at`, `request_id`, and the plan's `summary` |
| `GET` | `/revisions/{rev}` | The config at that revision |
| `POST` | `/catalog` | `{ kinds }` → the generated identity and audit routes for those kinds, as `services`. Writes nothing |
| `GET` | `/schema` | The JSON Schema for the config and each document |

## Responses

Validation, auth and empty-body failures use the Plat5 envelope ([`reference/errors.md`](../errors.md)) and write nothing.

| Case | Status |
|------|--------|
| Dry run | **200**, the plan |
| Applied | **200** `{ "revision": 42, "summary": {…} }`. Readers see it on their next poll |
| Schema or cross-document rule broken, unknown key, blocked change | **422** `VALIDATION_ERROR`, naming the field path, such as `services.e10s.routes[2].scope`. The CLI adds the file |
| A method + path shape owned by two services | **409** `ROUTE_CONFLICT` ([`reference/config/routes.md`](../config/routes.md#route-ownership)) |
| A precondition failed | **409** `PLAN_STALE` |
| `expect` missing | **422** |
| Identity unreachable while computing a plan that removes a kind or narrows a parent | **503**: that plan needs its counts |
| Postgres failure | **503**. Nothing written |

## Seeds

On first boot there's nothing to destroy, so the seed applies without `confirm`. Dev compose mounts `compose/seed/` (`plat5.yml` and the generated `routes/identity.yml` and `routes/audit.yml`) and sets `SEED_DIR`. Prod compose mounts a `plat5.yml` with kinds and roles and an empty `services`, so identity can start; apply the rest yourself. After the first apply the folder is never read again.
