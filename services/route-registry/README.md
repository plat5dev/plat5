# Route registry

Admin HTTP service: validate route configs, expand prefixes, write to etcd for the Plat5 gateway.

Contract: [`../../docs/route-registry.md`](../../docs/route-registry.md).

## Ports

| Port | Env | Purpose |
|------|-----|---------|
| 5002 | `PORT` | Admin API |
| 5003 | `INTERNAL_PORT` | `/health/live`, `/health/ready`, `/metrics` |

## Telemetry

Contract: [`../../docs/telemetry.md`](../../docs/telemetry.md).

| Signal | Path |
|--------|------|
| Logs | JSON stdout (tracing); access line per request |
| Metrics scrape | Prometheus `/metrics` on `INTERNAL_PORT` |
| Traces | OTLP HTTP when endpoint set (default) |
| Metrics OTLP | On when endpoint set (default); set `OTEL_METRICS_EXPORTER=prometheus` to opt out |

| Var | Default | Purpose |
|-----|---------|---------|
| `OTEL_SERVICE_NAME` | `route-registry` | Resource `service.name` |
| `OTEL_SERVICE_NAMESPACE` | `edge` | Resource `service.namespace` |
| `OTEL_SERVICE_VERSION` | crate version | Resource `service.version` |
| `OTEL_SERVICE_INSTANCE_ID` | hostname / pid | Resource `service.instance.id` |
| `DEPLOYMENT_ENV` / `OTEL_DEPLOYMENT_ENV` | `development` | Resource `deployment.environment` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | unset | OTLP base URL. Unset → no OTLP |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | unset | Optional full traces URL |
| `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | unset | Optional full metrics URL |
| `OTEL_TRACES_EXPORTER` | `otlp` when endpoint set | Include `otlp` to push traces |
| `OTEL_METRICS_EXPORTER` | `otlp` when endpoint set | Set `prometheus` to push-off; `/metrics` always on |
| `OTEL_METRIC_EXPORT_INTERVAL` | SDK default | ms (OTLP metrics only) |
| `OTEL_TRACES_SAMPLER_RATIO` | `1` | Trace sampling ratio |
| `OTEL_SDK_DISABLED` | unset | `true` → no OTLP; stdout + `/metrics` remain |

```bash
# traces push + scrape metrics (no double count)
OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318

# full OTLP push — do not also scrape /metrics into the same backend
# OTEL_METRICS_EXPORTER=prometheus  # opt out of metrics push
```

Health and `/metrics` are on the internal app (not request-traced).

## Run (compose)

Started with Plat5 compose. Dev admin API: `http://localhost:5002`.

```bash
export ADMIN_TOKEN=dev-admin-token
curl -sS http://localhost:5002/v1/services \
  -H "Authorization: Bearer $ADMIN_TOKEN"
```

## Apply routes

```bash
curl -sS -X POST http://localhost:5002/v1/apply \
  -H "Authorization: Bearer dev-admin-token" \
  -H "Content-Type: application/yaml" \
  --data-binary @./routes.yml
```

## Local cargo

```bash
cd services/route-registry
export ETCD_URL=http://localhost:2379
export ADMIN_TOKEN=dev-admin-token
cargo run
cargo test
cargo build
```
