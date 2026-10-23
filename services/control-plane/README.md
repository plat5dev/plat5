# Control plane

The deployment's config: kinds, roles, routes and CORS origins, applied as one whole config. A dry run answers the plan; an apply recomputes it and writes one revision if its preconditions hold. Serves the current config to the gateway and identity over long-poll.

Contract: [`../../docs/concepts/config.md`](../../docs/concepts/config.md), [`../../docs/reference/api/control-plane.md`](../../docs/reference/api/control-plane.md). The config's JSON Schema is [`../../schema/v2/`](../../schema/v2/), compiled in (`build.rs`).

## Ports

| Port | Env | Purpose |
|------|-----|---------|
| 5002 | `PORT` | Admin API (Bearer `ADMIN_TOKEN`) |
| 5003 | `INTERNAL_PORT` | `/internal/gateway`, `/internal/identity`, `/health/live`, `/health/ready`, `/metrics` |

`IDENTITY_INTERNAL_URL` is required: a plan that removes a kind or narrows a parent asks identity for counts. `SEED_DIR` holds a `plat5.yml` and its routes files, applied when the head is `0`.

## Layout

| File | |
|------|--|
| `src/config.rs` | The config's types and the stored form the feeds serve |
| `src/schema.rs` | `schema/v2/`, compiled in; errors as field paths |
| `src/rules.rs` | The seven cross-document rules, route shape, route ownership |
| `src/plan.rs` | The plan, its classification, and the apply's preconditions |
| `src/catalog.rs` | Identity's and audit's generated routes (`POST /catalog`) |
| `src/apply.rs`, `src/store.rs` | Apply with the head locked; revisions in Postgres |
| `fixtures/plans/` | Fixture configs and the plan each must produce |
| `fixtures/catalog/` | Kinds and roles, and the identity feed holding their generated catalog, which identity's tests walk |
| `src/docgen.rs` | Test: `docs/reference/config/` field tables match the schema |

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
| `OTEL_SERVICE_NAME` | `control-plane` | Resource `service.name` |
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

Health and `/metrics` are not request-traced. The `/internal/*` reads are.

## Run (compose)

Started with Plat5 compose. Dev admin API: `http://localhost:5402`.

```bash
export ADMIN_TOKEN=dev-admin-token
curl -sS http://localhost:5402/config -H "Authorization: Bearer $ADMIN_TOKEN"
```

Change the config with the CLI (`plat5 plan`, `plat5 apply`), not by hand.

## Local cargo

```bash
cd services/control-plane
export DATABASE_URL=postgres://plat5:plat5@localhost:5432/plat5?sslmode=disable
export ADMIN_TOKEN=dev-admin-token
export IDENTITY_INTERNAL_URL=http://localhost:3001
cargo run
cargo test                                   # TEST_DATABASE_URL=… also runs the API against Postgres
UPDATE_DOCS=1 cargo test docs                # rewrite the docs' generated field tables
UPDATE_FIXTURES=1 cargo test catalog         # rewrite fixtures/catalog/identity-feed.json
```
