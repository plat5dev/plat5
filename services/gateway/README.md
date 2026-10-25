# Gateway

Rust reverse proxy built on Pingora. It matches a route, reads the node chain from the path, authenticates (`Authorization: Bearer`: a JWT verified locally, anything else a key for identity), and calls identity's admit (cached) for the caller's member and permissions on that node. Then it applies the route's rate-limit policies (Valkey, all at once), checks `required_permissions`, fills `upstream` and the `X-Plat5-*` headers, and writes the audit intent. It strips `Authorization` and any client `X-Plat5-*`, and handles trace propagation, CORS, security headers and body limits. TLS is terminated at the edge (not in this process). How a request is decided: [`docs/concepts/access.md`](../../docs/concepts/access.md#how-a-request-is-decided); the contract: [`docs/reference/gateway.md`](../../docs/reference/gateway.md).

## Local Development

Requires stable Rust (install via `rustup`).

```bash
cd services/gateway

cargo fmt
cargo clippy --all-targets -- -Dwarnings
cargo test --all-targets
```

The pipeline tests (`src/pipeline/tests.rs`) run every route of the catalog fixture, `../control-plane/fixtures/catalog/gateway-feed.json`, through match, admit, limits, the permission check and the upstream fill. `control_plane::tests::every_accepted_fixture_feed_builds` builds every feed the control plane's fixtures accept (the catalog and each plan case), so a config apply accepts is one the gateway can build.

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `PORT` | `5001` | Public HTTP proxy port |
| `INTERNAL_PORT` | `8000` | Internal port for `/health` and `/metrics` |
| `CONTROL_PLANE_URL` | (required) | Control plane's internal base URL, e.g. `http://control-plane:5003`. Routes, rate limits and CORS come from `…/internal/gateway` |
| `IDENTITY_INTERNAL_URL` | (required) | Identity's internal base URL, e.g. `http://identity:3001`; admit is at `/internal/admit` |
| `VALKEY_URL` | (required) | Valkey for rate-limit counters. Replicas share one budget |
| `AUTH_ISSUER` | (required) | JWT `iss` |
| `AUTH_JWKS_URI` | (required) | JWKS URL |
| `AUTH_ALLOWED_AUDIENCES` | (required) | Comma-separated allowed `aud` values; `*` accepts any audience |
| `AUTH_SUBJECT_CLAIM` | `sub` | Dotted claim path for the person's IdP subject (e.g. `properties.user_id`). The name claim is `users.name_claim` in `plat5.yml` |
| `ADMIT_CACHE_TTL` | `5m` | How long an admit answer is cached, a duration |
| `ADMIT_NEGATIVE_CACHE_TTL` | `10s` | How long a refusal (`valid: false`, `member_id: null`) is cached |
| `AUDIT_ENABLED` | `true` | Wiring: whether the deployment runs an audit service. Anything but `true` / `false` refuses boot |
| `AUDIT_URL` | (required unless audit is off) | Audit's internal base URL, e.g. `http://audit:3003` ([`docs/concepts/audit.md`](../../docs/concepts/audit.md)) |
| `RATE_LIMIT_AUTH_FAILURE_REQUESTS` | `60` | Failed-auth IP limiter. `0` = off |
| `RATE_LIMIT_AUTH_FAILURE_WINDOW_SECONDS` | `60` | Failed-auth IP window |
| `TRUSTED_PROXIES` | (empty) | Comma-separated addresses and CIDRs of proxies in front of the gateway. Their `X-Forwarded-For` names the client. Empty = the TCP peer is the client ([`docs/reference/gateway.md`](../../docs/reference/gateway.md#client-ip)) |
| `OTEL_SERVICE_NAME` | `gateway` | Resource `service.name` |
| `OTEL_SERVICE_NAMESPACE` | `edge` | Resource `service.namespace` |
| `OTEL_SERVICE_VERSION` | crate version | Resource `service.version` |
| `OTEL_SERVICE_INSTANCE_ID` | hostname | Resource `service.instance.id`, and the replica id reported to the control plane |
| `DEPLOYMENT_ENV` / `OTEL_DEPLOYMENT_ENV` | `development` | Resource `deployment.environment` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | unset | OTLP base URL. Unset → no OTLP |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | unset | Optional full traces URL |
| `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | unset | Optional full metrics URL |
| `OTEL_TRACES_EXPORTER` | `otlp` when endpoint set | Include `otlp` to push traces |
| `OTEL_METRICS_EXPORTER` | `otlp` when endpoint set | Set `prometheus` to push-off; `/metrics` always on |
| `OTEL_METRIC_EXPORT_INTERVAL` | SDK default | ms (OTLP metrics only) |
| `OTEL_TRACES_SAMPLER_RATIO` | `1` | Trace sampling ratio |
| `OTEL_SDK_DISABLED` | unset | `true` → no OTLP; stdout + `/metrics` remain |

Audit: on an audited kind or user route, the intent (the node chain, and who acted) is written after the permission check and the fill, before forward (up to 3 attempts in 1s), or the request is **503** and not forwarded. The outcome is queued after the response and retried for about five minutes; a full queue drops it and the event stays `pending`. A permission **403** or a param **400** on an audited route is one background `rejected` write. Client `X-Plat5-*` headers are stripped from every request, and `X-Plat5-Audit-*` from every response. Audit is not part of `/health/ready`.

Rate limits: a route's policies are checked together in one Valkey round trip and counted only if all pass. A policy counts each caller at the route's node, or everyone at its `count_at` node; user routes count by user id, `public` by client IP. Keys sit under `plat5:rl:`. Valkey error or timeout on a limited request → **503**. The gateway reconnects when Valkey answers again. Admitted limited routes set `X-RateLimit-Limit` / `Remaining` / `Reset` for the tightest policy.

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

Routes, rate limits and CORS come from the **control plane**. The gateway waits for the first snapshot at boot, then long-polls for changes and keeps the last snapshot while the control plane is down. The control plane validated each snapshot; the gateway parses it and builds its route table, and one it can't is refused whole: the current one stays, `gateway_config_refused_total{revision}` counts it, and the next poll reports it (`replica`, `loaded`, `refused`, `reason`). `gateway_config_revision` is the revision the replica serves. See [`docs/reference/gateway.md`](../../docs/reference/gateway.md#control-plane) and [`docs/reference/config/services.md`](../../docs/reference/config/services.md).

## Health

- `/health/live` — process up (always 200 when serving). The gateway serves nothing until it has a control-plane snapshot.
- `/health/ready` — 200 when JWKS is loaded and Valkey answers PING within 500ms; **503** otherwise (do not send traffic until ready).

## Span Status

- **Client errors** (400, 401, 403, 404, 413, 429): span status unset.
- **Unexpected failures** (5xx, proxy/network errors): span status `Error`, set `error.kind`.
