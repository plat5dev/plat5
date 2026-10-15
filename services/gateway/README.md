# Gateway

Rust reverse proxy built on Pingora. Handles request routing, JWT / API key / member session authentication, subject fill into `upstream`, route labels (`required_labels`) against the member's role labels, stripping of `Authorization` / `X-API-Key` before upstream, rate limits (Valkey), the org audit log's intent and outcome writes, trace propagation, CORS, and security headers. TLS is terminated at the edge (not in this process).

## Local Development

Requires stable Rust (install via `rustup`).

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
| `CONTROL_PLANE_URL` | (required) | Control plane's internal base URL, e.g. `http://control-plane:5003`. Routes and CORS origins come from `…/internal/gateway` |
| `VALKEY_URL` | (required) | Valkey for rate-limit counters. Replicas share one budget |
| `AUTH_ISSUER` | (required) | JWT `iss` |
| `AUTH_JWKS_URI` | (required) | JWKS URL |
| `AUTH_ALLOWED_AUDIENCES` | (empty) | Comma-separated allowed `aud` values |
| `AUTH_USER_ID_CLAIM` | `properties.user_id` | Dotted claim path for the person's IdP subject (`sub` for many OIDC IdPs). Identity resolves issuer + subject to a `user_id` |
| `CREDENTIAL_VALIDATE_URL` | (required) | Identity's one validate (`…/internal/credentials/validate`): every `X-API-Key`, and every JWT's issuer and subject |
| `APIKEY_CACHE_TTL_SECS` | `300` | Credential and IdP subject cache TTL (valid and invalid results) |
| `INTERNAL_AUTH_TOKEN` | unset | Sent as `X-Plat5-Internal-Token` to validate, audit, and the control plane when set |
| `AUDIT_ENABLED` | `true` | `false` turns audit off for the deployment. Anything but `true` / `false` refuses boot |
| `AUDIT_URL` | (required unless audit is off) | Audit's internal base URL, e.g. `http://audit:3003` ([`docs/audit.md`](../../docs/audit.md)) |
| `RATE_LIMIT_REQUESTS` | `60` | Fallback per-route limit. `0` = unlimited fallback |
| `RATE_LIMIT_WINDOW_SECONDS` | `60` | Fallback window |
| `RATE_LIMIT_AUTH_FAILURE_REQUESTS` | `60` | Failed-auth IP limiter. `0` = off |
| `RATE_LIMIT_AUTH_FAILURE_WINDOW_SECONDS` | `60` | Failed-auth IP window |
| `TRUSTED_PROXIES` | (empty) | Comma-separated addresses and CIDRs of proxies in front of the gateway. Their `X-Forwarded-For` names the client. Empty = the TCP peer is the client ([`docs/gateway-contract.md`](../../docs/gateway-contract.md#client-ip)) |
| `UPSTREAM_CONNECT_TIMEOUT_MS` | `10000` | Upstream connection timeout, for a service whose route config sets no `timeouts.connect_ms` |
| `UPSTREAM_READ_TIMEOUT_MS` | `30000` | Upstream read timeout, for a service whose route config sets no `timeouts.read_ms` |
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

Audit: on an audited `organization` or `member` route, the intent is written before forward (up to 3 attempts in 1s) or the request is **503** and not forwarded. The outcome is queued after the response and retried for about five minutes; a full queue drops it and the event stays `pending`. `X-Plat5-Audit-Details` is stripped from every request and response. Audit is not part of `/health/ready`.

Limiter subject follows route scope: `public`→ip, `user`→user, `organization`→org, `member`→member. Valkey error or timeout on a limited request → **503**. The gateway reconnects when Valkey answers again. Admitted limited routes set `X-RateLimit-Limit` / `Remaining` / `Reset`.

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

## Control plane

Routes and CORS origins come from the **control plane**. The gateway waits for the first snapshot at boot, then long-polls for changes and keeps the last snapshot while the control plane is down. CORS origins are not env: set them with `PUT /cors`. See [`docs/routes.md`](../../docs/routes.md) and [`docs/control-plane.md`](../../docs/control-plane.md).

## Health

- `/health/live` — process up (always 200 when serving). The gateway serves nothing until it has a control-plane snapshot.
- `/health/ready` — 200 when JWKS is loaded and Valkey answers PING within 500ms; **503** otherwise (do not send traffic until ready).

## Span Status

- **Client errors** (400, 401, 403, 404, 413, 429): span status unset.
- **Unexpected failures** (5xx, proxy/network errors): span status `Error`, set `error.kind`.
