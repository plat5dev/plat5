# API Keys Service

Go Fiber service for API key management. Postgres-backed (`api_keys` schema on Plat5 DB).

## Local Development

Requires Go 1.24+ and Postgres.

```bash
cd services/api-keys

export DATABASE_URL=postgres://plat5:plat5@localhost:5432/plat5?sslmode=disable
PORT=3000 go run .

go build ./...
go test ./...
gofmt -w .
go vet ./...
```

## Storage

Database: Plat5 Postgres via `DATABASE_URL`.  
Schema: **`api_keys`** (service-owned; `search_path` set on connect).

| Table | Purpose |
|-------|---------|
| `api_keys` | Hashed keys, user index, revoke timestamp |
| `schema_migrations` | Applied migration versions |

Key ids are **ULID**. Plaintext key returned only once on create (`plat5-sk-1-…`). Stored as SHA-256 hex hash.

## Env

| Var | Default | Purpose |
|-----|---------|---------|
| `PORT` | `3000` | Public API |
| `INTERNAL_PORT` | `3001` | Health, `/metrics`, internal validate |
| `INTERNAL_AUTH_TOKEN` | unset | When set, require `X-Plat5-Internal-Token` on validate |
| `DATABASE_URL` | local postgres URL | Identity DB |
| `OTEL_SERVICE_NAME` | `api-keys` | Resource `service.name` |
| `OTEL_SERVICE_NAMESPACE` | `identity` | Resource `service.namespace` |
| `OTEL_SERVICE_VERSION` | `0.0.0` / `CI_COMMIT_TAG` | Resource `service.version` |
| `OTEL_SERVICE_INSTANCE_ID` | `HOSTNAME` | Resource `service.instance.id` |
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

Contract: Plat5 `docs/telemetry.md`.

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

Ready probe fails closed (**503**) when Postgres is unreachable. Health and `/metrics` are on the internal app (not OTel-instrumented).

## Validate (gateway)

Internal listener only. Contract: [`docs/api-keys.md`](../../docs/api-keys.md).

```
POST /internal/keys/validate
X-Plat5-Internal-Token: <token>   # when INTERNAL_AUTH_TOKEN set
{ "key": "plat5-sk-1-…" }
→ { "valid": true, "user_id": "…" } | { "valid": false }
```

Not in `routes.yml`. Not on `PORT`.
