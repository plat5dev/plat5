# Health Check Contract

Expose `/health/live` and `/health/ready` on a separate internal port for orchestrator probes.

## Endpoints

- `/health/live` — Liveness. Returns 200 if the process is running. Keep it cheap.
- `/health/ready` — Readiness. Returns 200 if the service can accept traffic and critical dependencies are reachable.

## Response Format

```json
{
  "status": "healthy"
}
```

| Status | Meaning | HTTP Code |
|--------|---------|-----------|
| `healthy` | Running, can accept traffic, all critical dependencies reachable | 200 |
| `degraded` | Running, can accept traffic, non-critical dependencies unhealthy | 200 |
| `unhealthy` | Cannot accept traffic; critical dependency down | 503 |

Services may include additional fields (e.g. `uptime_ms`, `version`) for debugging.

## Best Practices

- Keep checks fast (< 100ms).
- Do not expose sensitive state.
- Do not instrument health checks with OpenTelemetry (avoids trace noise).

## Internal Port

Serve health checks (and metrics) on a separate internal port. Use the `INTERNAL_PORT` env var.

## Docker Compose

```yaml
healthcheck:
  test: ["CMD", "curl", "-f", "http://localhost:${INTERNAL_PORT}/health/ready"]
  interval: 30s
  timeout: 10s
  retries: 3
  start_period: 10s
```
