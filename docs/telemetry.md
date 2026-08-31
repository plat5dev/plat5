# Telemetry contract

Logs, traces, and metrics for Plat5 services. Plat5 does **not** ship a collector. Operators bring any OTLP-compatible collector (or scrape Prometheus) — Alloy, otel-collector, Grafana Cloud OTLP, etc.

## Standard (summary)

| Signal | Always | Opt-in |
|--------|--------|--------|
| **Logs** | JSON to **stdout** | — (no OTLP logs) |
| **Metrics scrape** | Prometheus **`/metrics`** on the internal port | — |
| **Traces OTLP** | — | When endpoint is set and traces exporter includes `otlp` |
| **Metrics OTLP** | — | When endpoint is set **and** metrics exporter includes `otlp` |

**Defaults when an OTLP endpoint is set:**

- Traces → OTLP on (`OTEL_TRACES_EXPORTER` unset defaults to `otlp`)
- Metrics → OTLP on (`OTEL_METRICS_EXPORTER` unset defaults to `otlp`)
- `/metrics` scrape → still on

Set `OTEL_METRICS_EXPORTER=prometheus` (or `none`) to push traces only and scrape metrics.

**Do not** scrape `/metrics` into the same backend you also feed via OTLP metrics for the same series.

## Configuration

Prefer standard OpenTelemetry environment variables for exporters and destinations. Do not invent project-specific exporter endpoint vars as the primary API.

### Destination

| Variable | Purpose |
|----------|---------|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Collector base URL (e.g. `http://localhost:4318`). Unset = no OTLP destination. |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | Optional full traces URL (overrides base + `/v1/traces`) |
| `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | Optional full metrics URL (overrides base + `/v1/metrics`) |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | **`http/protobuf` only** (OTLP/HTTP). `grpc` is not supported. |
| `OTEL_EXPORTER_OTLP_HEADERS` | Optional headers (e.g. Grafana Cloud auth). Honored by the OTLP exporter SDKs. |
| `OTEL_EXPORTER_OTLP_*_HEADERS` | Per-signal header overrides when needed |

Endpoint = **where** to send OTLP. When set, traces and metrics both default to OTLP push.

### Exporter selection

| Variable | Default | Meaning |
|----------|---------|---------|
| `OTEL_TRACES_EXPORTER` | `otlp` if endpoint set, else effectively `none` | Comma-separated. OTLP traces only if value includes `otlp` **and** a traces destination exists. |
| `OTEL_METRICS_EXPORTER` | `otlp` if endpoint set | Comma-separated. OTLP metrics only if value includes `otlp` **and** a metrics destination exists. `/metrics` stays up regardless (see below). |
| `OTEL_SDK_DISABLED` | unset | `true` → no OTLP export. Stdout logs and `/metrics` remain. |

| `OTEL_METRICS_EXPORTER` | OTLP metrics | `/metrics` |
|------------------------|--------------|------------|
| unset (default) | on (needs endpoint) | on |
| `otlp` | on (needs endpoint) | on |
| `otlp,prometheus` | on | on |
| `prometheus` | off | on |
| `none` | off | on |

`none` means no metrics **push**, not “disable scrape.”

### Resource identity

**Standard OTel** ([SDK env spec](https://opentelemetry.io/docs/specs/otel/configuration/sdk-environment-variables/)):

| Variable | Purpose |
|----------|---------|
| `OTEL_SERVICE_NAME` | Resource `service.name` (takes precedence over the same key in `OTEL_RESOURCE_ATTRIBUTES`) |
| `OTEL_RESOURCE_ATTRIBUTES` | Comma-separated `key=value` resource attributes |

Example:

```bash
OTEL_SERVICE_NAME=gateway
OTEL_RESOURCE_ATTRIBUTES=service.namespace=edge,service.version=1.2.3,service.instance.id=gw-1,deployment.environment=dev
```

**Project convenience** (not dedicated OTel env vars; services also read these and map them onto the same resource attributes — useful in compose):

| Variable | Maps to |
|----------|---------|
| `OTEL_SERVICE_NAMESPACE` | `service.namespace` |
| `OTEL_SERVICE_VERSION` | `service.version` |
| `OTEL_SERVICE_INSTANCE_ID` | `service.instance.id` (fallback: `HOSTNAME`) |
| `DEPLOYMENT_ENV` / `OTEL_DEPLOYMENT_ENV` | `deployment.environment` |

If both `OTEL_RESOURCE_ATTRIBUTES` and a convenience var set the same attribute, the convenience var wins. Prefer `OTEL_RESOURCE_ATTRIBUTES` for portable operator config; convenience vars are fine for our compose defaults. All services honor both.

`DEPLOYMENT_ENV` is also used for non-telemetry behavior in some products (e.g. Auth disables `/dev/token` when `prod`).

### Other

| Variable | Purpose |
|----------|---------|
| `OTEL_TRACES_SAMPLER_RATIO` | Trace sampling ratio when simple ratio sampler is used |
| `OTEL_METRIC_EXPORT_INTERVAL` | OTLP metric export interval (ms) |

Default Plat5 compose does **not** set `OTEL_EXPORTER_OTLP_ENDPOINT`.

## Operator recipes

### Full OTLP push (e.g. Grafana Cloud / local LGTM kit)

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
# traces + metrics OTLP on by default when endpoint is set
# Do not also scrape /metrics into the same metrics backend
```

### Traces push + metrics scrape (no double count)

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
export OTEL_METRICS_EXPORTER=prometheus   # opt out of metrics push
# scrape http://<host>:<internal_port>/metrics
```

### Scrape only (no traces)

Leave `OTEL_EXPORTER_OTLP_ENDPOINT` unset. Scrape `/metrics`. Collect stdout logs in the platform.

### Local all-in-one collector

```bash
docker run --rm -p 3000:3000 -p 4317:4317 -p 4318:4318 \
  --name otel-lgtm grafana/otel-lgtm:latest
```

Point services at `http://localhost:4318` (host) or `http://host.docker.internal:4318` (containers → host). Any OTLP collector on `:4318` works (not part of this product).

## Logs

Write **JSON** to stdout always (including when OTLP is enabled). No OTLP log export.

```json
{"timestamp":"2024-01-15T10:30:00Z","level":"info","message":"request completed","route":"/api/test","method":"GET","status":200,"duration_ms":12.5}
```

### Common fields

| Field | Description |
|-------|-------------|
| `timestamp` | ISO8601 |
| `level` | debug, info, warn, error |
| `message` | Log message |
| `route` | HTTP route pattern |
| `method` | HTTP method |
| `status` | HTTP status code |
| `duration_ms` | Request duration |
| `request_id` | Correlation ID |
| `trace_id` / `span_id` | When a span is active |
| `user_id` | Authenticated user ID (when known) |
| `organization_id` | Organization ID (when in org context) |
| `member_id` | Member ID (when in org context) |

Field names: [`naming-conventions.md`](naming-conventions.md).

### Error fields (5xx only)

| Field | Description |
|-------|-------------|
| `error_kind` | `auth`, `network`, `db`, `io`, `internal`, `validation` |
| `error_message` | Human-readable message |

Do not include `error_kind` for 4xx.

## Traces

- Export via **OTLP** only (no Jaeger/Zipkin native APIs from the app).
- Propagate **W3C Trace Context** only (no Baggage).
- Resource attributes on every export: `service.name`, `service.namespace`, `service.instance.id`, `service.version`, `deployment.environment`.

### HTTP server spans

Stable [HTTP span conventions](https://opentelemetry.io/docs/specs/semconv/http/http-spans/). Do **not** emit deprecated names. Do **not** dual-write old and new.

| | |
|---|---|
| Kind | `SERVER` |
| Name | `{method} {http.route}` when a framework route template exists; otherwise `{method}` only. Never the raw URI. |
| Required | `http.request.method`, `url.path`, `url.scheme` |
| When known | `http.response.status_code`, `http.route` (template only), `url.query` if present, `error.type` on 5xx (status code as a string, e.g. `"500"`) |
| Status | Unset on 4xx. `Error` on 5xx with **no** description (infer from `http.response.status_code`). Unset on 1xx/2xx/3xx unless a non-HTTP error occurred. |

**Forbidden on spans:** `http.method`, `http.status_code`, `http.target`, `http.url`, `http.scheme`.

`http.route` is the matched template (`/api/organizations/:organization_id/subscribers`). Do not put the raw path there. `url.path` is the actual path (IDs allowed).

Gateway creates the root span **before** route match: name starts as `{method}`, then becomes `{method} {template}` after match. Unmatched stays `{method}`. Internal child spans (`gateway.request_filter`) are not HTTP server spans.

### Plat5 span attributes (not HTTP semconv)

- `request_id` on HTTP request spans (if present)
- `user.id` on HTTP request spans when authenticated — set at the **edge/gateway** for ops. Org-scoped services do not receive `X-User-Id`; they should not invent `user.id`. Gateway may also set `organization.id` / `member.id`.
- `error.kind` on **error spans** (5xx only): `auth`, `network`, `db`, `io`, `internal`, `validation`
- 4xx responses are normal business outcomes — do not set `error.kind` and do not mark the span as failed
- Record exceptions via `span.recordException(err)` / `span.record_exception(err)`

`error.kind` is our taxonomy. `error.type` is HTTP semconv. Both on 5xx. Do not merge them. Do not rename `request_id` to `request.id`.

Logs and Prometheus metrics are **not** HTTP spans: log fields stay `snake_case` (`request_id`, `method`, `status`); metric names stay `http_requests_total` / `http_request_duration_seconds`.

## Metrics

### Scrape (always)

Expose Prometheus text format at **`/metrics`** on the **internal** port (not public ingress). See [`health-checks.md`](health-checks.md) / service READMEs for ports. Optional discovery labels: [`container-labels.md`](container-labels.md) (`metrics.port`, `metrics.path`).

### OTLP (default when endpoint set)

When a metrics destination is set and `OTEL_METRICS_EXPORTER` is unset or includes `otlp`, push the **same logical series** via OTLP (same base endpoint → `/v1/metrics` unless overridden).

### Cardinality

**Never** put high-cardinality values in metric labels (user IDs, request IDs, emails). Those belong on spans and logs.

### Naming

- `snake_case`
- Prefix by domain: `http_`, `db_`, `process_`, `auth_`
- Suffix by type: `_total`, `_seconds`, `_bytes`

| Metric | Type | Labels |
|--------|------|--------|
| `http_requests_total` | Counter | route, method, status |
| `http_request_duration_seconds` | Histogram | route, method |
| `db_operations_total` | Counter | db_system_name, db_operation_name, db_namespace |
| `db_operation_errors_total` | Counter | db_system_name, db_operation_name, db_namespace |
| `db_operation_duration_seconds` | Histogram | db_system_name, db_operation_name, db_namespace |

**Histogram buckets** (same across all services):

| Metric | Boundaries (seconds) |
|--------|----------------------|
| `http_request_duration_seconds` | `0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5` |
| `db_operation_duration_seconds` | `0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1` |

Do not include `service_name` / `service_namespace` in metric series if your collector already adds them from resource attributes or container labels.

### Process metrics (minimum)

Always emit on the scrape path (language standard collectors preferred):

| Metric | Notes |
|--------|--------|
| `process_resident_memory_bytes` | RSS |
| `process_cpu_seconds_total` | Cumulative user+system CPU |
| `process_start_time_seconds` | Unix epoch start time |

When OTLP metrics are enabled, the same series should appear there too. Extra process/runtime series (heap, FDs, etc.) are optional.

## Service expectations

| Service | Namespace | Notes |
|---------|-----------|--------|
| gateway | `edge` | Internal `/metrics`; gateway sets edge span identity attrs |
| route-registry | `edge` | Internal `/metrics` |
| identity | `identity` | Internal `/metrics` |

## Non-goals

- Shipping or requiring a collector / Alloy config as part of the product
- OTLP log export from apps
- App push to Loki, Tempo, or Mimir **native** APIs
- Public exposure of `/metrics`
- Custom non-`OTEL_*` env vars as the primary telemetry API
- HTTP **metric** semconv (`http.server.request.duration`) — scrape/OTLP series stay `http_requests_total` / `http_request_duration_seconds`

## References

- [OTel SDK environment variables](https://opentelemetry.io/docs/specs/otel/configuration/sdk-environment-variables/)
- [OTLP exporter configuration](https://opentelemetry.io/docs/languages/sdk-configuration/otlp-exporter/)
- [HTTP span semantic conventions](https://opentelemetry.io/docs/specs/semconv/http/http-spans/)
- [Semantic conventions](https://opentelemetry.io/docs/specs/semconv/)
- [grafana/otel-lgtm](https://github.com/grafana/docker-otel-lgtm)
