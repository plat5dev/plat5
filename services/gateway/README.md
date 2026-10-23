# Gateway

Rust reverse proxy built on Pingora. It matches a route, reads the node chain from the path, authenticates (JWT or `X-API-Key`), and calls identity's admit (cached) for the caller's member and labels on that node. Then it applies the route's rate-limit policies (Valkey), writes the audit intent, checks `required_labels`, and fills the subject into `upstream` and the `X-Plat5-*` headers. It strips `Authorization`, `X-API-Key` and any client `X-Plat5-*`, and handles trace propagation, CORS and security headers. TLS is terminated at the edge (not in this process). How a request is decided: [`docs/concepts/access.md`](../../docs/concepts/access.md#how-a-request-is-decided); the contract: [`docs/reference/gateway.md`](../../docs/reference/gateway.md).

## Local Development

Requires stable Rust (install via `rustup`).

```bash
cd services/gateway

cargo fmt
cargo clippy --all-targets -- -Dwarnings
cargo test --all-targets
```

The config schema is compiled in from the repo's `schema/` (`build.rs`; the Docker build takes it as the `schema` named context). The pipeline tests (`src/pipeline/tests.rs`) run every route of the catalog fixture, `../control-plane/fixtures/catalog/identity-feed.json`, through match, admit, limits, labels and the subject fill.

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `PORT` | `5001` | Public HTTP proxy port |
| `INTERNAL_PORT` | `8000` | Internal port for `/health` and `/metrics` |
| `CONTROL_PLANE_URL` | (required) | Control plane's internal base URL, e.g. `http://control-plane:5003`. Kinds, routes and CORS origins come from `…/internal/gateway` |
| `VALKEY_URL` | (required) | Valkey for rate-limit counters. Replicas share one budget |
| `AUTH_ISSUER` | (required) | JWT `iss` |
| `AUTH_JWKS_URI` | (required) | JWKS URL |
| `AUTH_ALLOWED_AUDIENCES` | (empty) | Comma-separated allowed `aud` values |
| `AUTH_SUBJECT_CLAIM` | `properties.user_id` | Dotted claim path for the person's IdP subject (`sub` for many OIDC IdPs). The name claim is the sign-in kind's `name_claim` |
| `ADMIT_URL` | (required) | Identity's admit (`…/internal/admit`): every credential, with the route's node chain |
| `APIKEY_CACHE_TTL_SECS` | `300` | Admit cache TTL, capped by each answer's `good_until` |
| `INTERNAL_AUTH_TOKEN` | unset | Sent as `X-Plat5-Internal-Token` to identity, audit, and the control plane when set |
| `AUDIT_ENABLED` | `true` | `false` turns audit off for the deployment. Anything but `true` / `false` refuses boot |
| `AUDIT_URL` | (required unless audit is off) | Audit's internal base URL, e.g. `http://audit:3003` ([`docs/concepts/audit.md`](../../docs/concepts/audit.md)) |
| `RATE_LIMIT_REQUESTS` | `60` | Fallback per-route limit. `0` = unlimited fallback |
| `RATE_LIMIT_WINDOW_SECONDS` | `60` | Fallback window |
| `RATE_LIMIT_AUTH_FAILURE_REQUESTS` | `60` | Failed-auth IP limiter. `0` = off |
| `RATE_LIMIT_AUTH_FAILURE_WINDOW_SECONDS` | `60` | Failed-auth IP window |
| `TRUSTED_PROXIES` | (empty) | Comma-separated addresses and CIDRs of proxies in front of the gateway. Their `X-Forwarded-For` names the client. Empty = the TCP peer is the client ([`docs/reference/gateway.md`](../../docs/reference/gateway.md#client-ip)) |
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

Audit: on an audited kind or sign-in route, the intent (the node chain, and the member who acted) is written before forward (up to 3 attempts in 1s) or the request is **503** and not forwarded. The outcome is queued after the response and retried for about five minutes; a full queue drops it and the event stays `pending`. Client `X-Plat5-*` headers are stripped from every request, and `X-Plat5-Audit-*` from every response. Audit is not part of `/health/ready`.

Rate limits: each policy on a route counts in the bucket of the node at its `count_at` (the fallback at the route's own node, `public` by client IP, sign-in routes by the directory member), and every one must pass. Valkey error or timeout on a limited request → **503**. The gateway reconnects when Valkey answers again. Admitted limited routes set `X-RateLimit-Limit` / `Remaining` / `Reset` for the tightest policy.

## Telemetry

Contract: [`docs/reference/telemetry.md`](../../docs/reference/telemetry.md).

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

Kinds, routes and CORS origins come from the **control plane**. The gateway waits for the first snapshot at boot, then long-polls for changes and keeps the last snapshot while the control plane is down. Each snapshot is checked against `schema/` and built whole; one that fails either is refused and the current one stays. CORS origins are not env: they're `cors` in `plat5.yml`. See [`docs/reference/gateway.md`](../../docs/reference/gateway.md#control-plane) and [`docs/reference/config/routes.md`](../../docs/reference/config/routes.md).

## Health

- `/health/live` — process up (always 200 when serving). The gateway serves nothing until it has a control-plane snapshot.
- `/health/ready` — 200 when JWKS is loaded and Valkey answers PING within 500ms; **503** otherwise (do not send traffic until ready).

## Span Status

- **Client errors** (400, 401, 403, 404, 413, 429): span status unset.
- **Unexpected failures** (5xx, proxy/network errors): span status `Error`, set `error.kind`.
