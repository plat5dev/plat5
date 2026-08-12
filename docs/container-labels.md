# Container Labels

Optional container labels for operators who scrape logs/metrics (e.g. their own collector). Plat5 does not require a specific collector.

## Recommended

| Label | Example | Purpose |
|-------|---------|---------|
| `service.name` | `identity` | Identifies the service in logs, traces, and metrics |
| `service.namespace` | `identity` | Groups related services |

## Metrics scrape (recommended)

Services expose Prometheus `/metrics` on the internal port ([`telemetry.md`](telemetry.md)). Label the container so operators can discover scrape targets:

| Label | Example | Purpose |
|-------|---------|---------|
| `metrics.port` | `3001` | Internal port serving Prometheus `/metrics` |
| `metrics.path` | `/metrics` | Path (default `/metrics` if omitted) |

## Namespace Values

| Namespace | Used By |
|-----------|---------|
| `identity` | Plat5 identity backend: `identity` |
| `edge` | Gateway (`gateway`), route registry (`route-registry`) |
| `api` | Business API services — **not** Plat5 identity |
| `infra` | Dependencies (etcd, postgres) |

### Platform service labels

| Service | `service.namespace` | Notes |
|---------|---------------------|--------|
| `identity` | **`identity`** | Orgs, members, service accounts, API keys |
| `gateway` | **`edge`** | Gateway hop |
| `route-registry` | **`edge`** | Route admin API |

When changing namespaces: update Docker Compose labels and `OTEL_SERVICE_NAMESPACE` together.

## Docker Compose Example

```yaml
services:
  my-service:
    image: my-service:latest
    labels:
      service.name: my-service
      service.namespace: api
      metrics.port: "3000"
    ports:
      - "8080:3000"
```

Platform backend:

```yaml
services:
  identity:
    labels:
      service.name: identity
      service.namespace: identity
      metrics.port: "3001"
    environment:
      OTEL_SERVICE_NAME: identity
      OTEL_SERVICE_NAMESPACE: identity
```

## Service Naming

See [`naming-conventions.md`](naming-conventions.md).
