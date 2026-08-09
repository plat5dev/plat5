# organizations

Plat5 service for organization lifecycle and memberships. Go (Fiber), Postgres.

## Responsibilities

- CRUD organizations
- Memberships (list, add, update role/status, remove/leave)
- Enforce at least one active owner
- Internal membership resolve for the gateway

Public routes are **`user` scope** only (`X-User-Id`). This service does **not** use `organization` scope.

Contract: [`docs/organizations.md`](../../docs/organizations.md)

## Local

```bash
# With stack
cd ../../compose && docker compose up --build organizations

# Or standalone (Postgres required)
export DATABASE_URL=postgres://plat5:plat5@localhost:5432/plat5?sslmode=disable
go run .
```

## Env

| Var | Default | Purpose |
|-----|---------|---------|
| `PORT` | `3000` | Public API |
| `INTERNAL_PORT` | `3001` | Health, `/metrics`, membership resolve |
| `INTERNAL_AUTH_TOKEN` | unset | When set, require `X-Plat5-Internal-Token` on resolve |
| `DATABASE_URL` | local postgres URL | Identity DB (`organizations` schema) |
| `OTEL_SERVICE_NAME` | `organizations` | Resource `service.name` |
| `OTEL_SERVICE_NAMESPACE` | `identity` | Resource `service.namespace` |
| `OTEL_SERVICE_VERSION` | `CI_COMMIT_TAG` / `0.0.0` | Resource `service.version` |
| `OTEL_SERVICE_INSTANCE_ID` | hostname / local | Resource `service.instance.id` |
| `DEPLOYMENT_ENV` / `OTEL_DEPLOYMENT_ENV` | `development` | Resource `deployment.environment` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | unset | OTLP base URL. Unset → no OTLP |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | unset | Optional full traces URL |
| `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | unset | Optional full metrics URL |
| `OTEL_TRACES_EXPORTER` | `otlp` when endpoint set | Include `otlp` to push traces |
| `OTEL_METRICS_EXPORTER` | `otlp` when endpoint set | Set `prometheus` to push-off; `/metrics` always on |
| `OTEL_METRIC_EXPORT_INTERVAL` | SDK default | ms (OTLP metrics only) |
| `OTEL_TRACES_SAMPLER_RATIO` | `1` | Trace sampling ratio |
| `OTEL_SDK_DISABLED` | unset | `true` → no OTLP; stdout + `/metrics` remain |

## Telemetry

Contract: [`docs/telemetry.md`](../../docs/telemetry.md).

| Signal | Path |
|--------|------|
| Logs | JSON stdout (zerolog); access line per request |
| Metrics scrape | Prometheus `/metrics` on `INTERNAL_PORT` |
| Traces | OTLP HTTP when endpoint set (default) |
| Metrics OTLP | On when endpoint set (default); set `OTEL_METRICS_EXPORTER=prometheus` to opt out |

```bash
# traces push + scrape metrics (no double count)
OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318

# full OTLP push — do not also scrape /metrics into the same backend
# OTEL_METRICS_EXPORTER=prometheus  # opt out of metrics push
```

Health and `/metrics` are on the internal app (not OTel-instrumented).

## Internal resolve

Internal listener only. Contract: [`docs/organizations.md`](../../docs/organizations.md).

```
POST /internal/memberships/resolve
X-Plat5-Internal-Token: <token>   # when INTERNAL_AUTH_TOKEN set
{ "user_id", "organization_id" }
→ { "membership_id", "organization_id", "user_id", "status" } | 404
```

Not published on the gateway. Not on `PORT`. Gateway admits only `status === "active"`.
