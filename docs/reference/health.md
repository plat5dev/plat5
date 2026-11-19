# Health checks

Every Plat5 runtime service exposes two endpoints on its `INTERNAL_PORT` for orchestrator probes:

- `GET /health/live` — Liveness. Returns **200** `{"status":"healthy"}` while the process is running.
- `GET /health/ready` — Readiness. Returns **200** `{"status":"healthy"}` when critical dependencies are reachable (Postgres, Valkey when rate limiting is enabled, JWKS, loaded config), otherwise **503** `{"status":"unhealthy"}`.

Health checks are not traced or counted in request metrics.
