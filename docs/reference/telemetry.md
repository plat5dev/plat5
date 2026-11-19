# Telemetry

How Plat5's runtime services emit logs, metrics, and traces — and what your services get if they follow the same trace and request-id headers.

Plat5 does not ship a collector. Bring any OTLP-compatible collector or Prometheus scraper.

## What Plat5 services emit

| Signal | Always on | Opt-in (when OTLP endpoint is set) |
|--------|-----------|------------------------------------|
| **Logs** | JSON to stdout | — (no OTLP log export) |
| **Metrics** | Prometheus `/metrics` on `INTERNAL_PORT` | OTLP push (`/v1/metrics`) unless opted out |
| **Traces** | W3C `traceparent` propagation | OTLP push (`/v1/traces`) |

### Configuration (standard OpenTelemetry env vars)

All Plat5 services use standard OpenTelemetry environment variables:

| Variable | Default / notes |
|----------|-----------------|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Collector base URL (e.g. `http://collector:4318`). Unset = no OTLP push |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` / `_METRICS_ENDPOINT` | Optional per-signal URL overrides |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | `http/protobuf` (OTLP/HTTP) |
| `OTEL_EXPORTER_OTLP_HEADERS` (and `_TRACES_` / `_METRICS_`) | Optional collector auth headers |
| `OTEL_TRACES_EXPORTER` | `otlp` when endpoint is set; set `none` to disable trace push |
| `OTEL_METRICS_EXPORTER` | `otlp` when endpoint is set; set `prometheus` or `none` to push traces only and scrape `/metrics` (don't both push and scrape into the same backend) |
| `OTEL_SDK_DISABLED` | `true` disables all OTLP export; stdout logs and `/metrics` remain |
| `OTEL_SERVICE_NAME` | Resource `service.name` |
| `OTEL_RESOURCE_ATTRIBUTES` | Standard comma-separated `key=value` attributes (e.g. `service.namespace=edge,deployment.environment=prod,service.instance.id=gw-1`) |
| `OTEL_TRACES_SAMPLER_RATIO` | Sampling ratio `0`–`1` |
| `OTEL_METRIC_EXPORT_INTERVAL` | OTLP metric export interval (ms) |

Invalid values for `OTEL_*` variables fail boot with an error naming the variable. Once running, export failures are logged and never stop the service.

### What's on logs, traces, and metrics

- **Logs:** Structured JSON on stdout (`timestamp`, `level`, `message`, `request_id`, `trace_id`, `span_id`, `route`, `method`, `status`, `duration_ms`, plus admitted ids like `root_id`, `node_id`, `member_id`, `user_id` — never secrets, tokens, or principal ids).
- **Traces:** Standard OTel HTTP server spans (`http.request.method`, `http.route`, `url.path`, `url.scheme`, `http.response.status_code`), with `request_id` and admitted `plat5.*` ids attached at the gateway. 4xx responses are normal outcomes (not span errors); 5xx mark the span as `Error`.
- **Metrics:** Prometheus format at `/metrics` on `INTERNAL_PORT`: HTTP request counters and latency histograms (`http_requests_total`, `http_request_duration_seconds`), DB operation metrics, process metrics, and key platform gauges/counters (`gateway_config_revision`, `gateway_config_refused_total`, `audit_writes_total`, `node_operations_total`, `access_evaluations_total`). High-cardinality ids never appear as metric labels.

## For your services (opt-in)

The gateway sets `X-Request-ID` and `traceparent` on every forwarded request. Your services can follow two lightweight conventions to get end-to-end observability across the gateway and your code:

- **Log `X-Request-ID` as `request_id`** → tie a single request together across gateway logs, audit events, error responses, and your service logs.
- **Read and propagate `traceparent`** (via any OpenTelemetry SDK) → see a single distributed trace from the edge through the gateway and into your handlers and databases.
