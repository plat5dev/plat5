# Gateway

Rust reverse proxy built on Pingora. Handles request routing, JWT/API key authentication, identity header injection, stripping of `Authorization` / `X-API-Key` before upstream, trace propagation, and CORS. TLS is terminated at the edge (not in this process).

## Local Development

Requires the Rust toolchain from `rust-toolchain.toml` (or default stable). Install via `rustup`.

```bash
cd services/gateway

cargo fmt
cargo clippy --all-targets -- -Dwarnings
cargo test --all-targets
```

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `PORT` | `5001` | Public HTTP proxy port |
| `INTERNAL_PORT` | `8000` | Internal port for `/health` and `/metrics` |
| `ETCD_URL` | `http://localhost:2379` | etcd endpoint for route registry |
| `AUTH_ISSUER` | (required) | JWT `iss` |
| `AUTH_JWKS_URI` | (required) | JWKS URL |
| `AUTH_ALLOWED_AUDIENCES` | (empty) | Comma-separated allowed `aud` values |
| `AUTH_USER_ID_CLAIM` | `properties.user_id` | Dotted claim path for Plat5 user id (`sub` for many OIDC IdPs) |
| `USER_APIKEY_VALIDATE_URL` | (required) | User key validate (`…/internal/user-keys/validate`); keys `plat5-sk-1-…` |
| `MEMBER_APIKEY_VALIDATE_URL` | (optional) | Member key validate (`…/internal/member-keys/validate`); keys `plat5-mk-1-…`; org scope (503 if unset when presented) |
| `APIKEY_CACHE_TTL_SECS` | `300` | User + member API key cache TTL |
| `MEMBERSHIP_RESOLVE_URL` | (optional) | Full URL for member resolve (`…/internal/members/resolve`); required for `organization` scope |
| `MEMBERSHIP_CACHE_TTL_SECS` | `300` | Member resolve cache TTL |
| `INTERNAL_AUTH_TOKEN` | unset | Sent as `X-Plat5-Internal-Token` to validate/resolve when set |
| `UPSTREAM_CONNECT_TIMEOUT_MS` | `10000` | Upstream connection timeout |
| `UPSTREAM_READ_TIMEOUT_MS` | `30000` | Upstream read timeout |
| `OTEL_SERVICE_NAME` | `gateway` | Resource `service.name` |
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
| `ALLOWED_ORIGINS` | (empty → `*`) | Comma-separated CORS origin allowlist. Empty allows `*`; non-empty reflects matching `Origin` and sets `Vary: Origin` |

## Telemetry

Contract: [`docs/telemetry.md`](../../docs/telemetry.md).

| Signal | Path |
|--------|------|
| Logs | JSON stdout (tracing) |
| Metrics scrape | Prometheus `/metrics` on `INTERNAL_PORT` |
| Traces | OTLP HTTP when endpoint set (default) |
| Metrics OTLP | On when endpoint set (default); set `OTEL_METRICS_EXPORTER=prometheus` to opt out |

```bash
# traces push + scrape metrics (no double count)
OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318

# full OTLP push — do not also scrape /metrics into the same backend
# OTEL_METRICS_EXPORTER=prometheus  # opt out of metrics push
```

Do not include `service_name` or `service_namespace` labels in Prometheus metrics if your collector adds them from container labels.

## Route Registry

The gateway loads route configuration from etcd (watch). Writes go through **route-registry**. See [`docs/routes.md`](../../docs/routes.md) and [`docs/route-registry.md`](../../docs/route-registry.md).

## Health

- `/health/live` — process up (always 200 when serving).
- `/health/ready` — 200 when JWKS is loaded; **503** when not (do not send traffic until ready).

## Span Status

- **Client errors** (400, 401, 404, 413): span status `Ok`.
- **Unexpected failures** (5xx, proxy/network errors): span status `Error`, set `error.kind`.
