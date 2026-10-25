# Control plane

The deployment's config: settings, kinds, roles, rate limits, CORS and services, applied as one whole config. A dry run answers the plan; an apply recomputes it and writes one revision if its precondition holds. Serves the current config to the gateway and identity over long-poll, with every service's routes expanded.

Contract: [`../../docs/concepts/config.md`](../../docs/concepts/config.md), [`../../docs/reference/api/control-plane.md`](../../docs/reference/api/control-plane.md), the feeds in [`../../docs/reference/api/internal.md`](../../docs/reference/api/internal.md#control-plane-feeds). The config's JSON Schema is [`../../schema/`](../../schema/), compiled in (`build.rs`).

## Ports

| Port | Env | Purpose |
|------|-----|---------|
| 5002 | `PORT` | Admin API (Bearer `ADMIN_TOKEN`; `GET /schema` open) |
| 5003 | `INTERNAL_PORT` | `/internal/gateway`, `/internal/identity`, `/health/live`, `/health/ready`, `/metrics`. No token: keep it inside the deployment's network |

`IDENTITY_INTERNAL_URL` is required: a plan that removes a kind or changes a kind's parent asks identity for counts. A fresh control plane has no config until the first `plat5 apply`.

## Layout

| File | |
|------|--|
| `src/config.rs` | The config's types |
| `src/schema.rs` | `schema/`, compiled in; errors as field paths |
| `src/resources.rs` | Identity's and audit's resources at their defaults |
| `src/expand.rs` | Every service's routes as the gateway matches them, overrides applied |
| `src/rules.rs` | The rules at apply and the route rules, over the expanded routes |
| `src/plan.rs` | The plan, line by line with before and after, and the apply's precondition |
| `src/counts.rs` | Identity's plan counts |
| `src/apply.rs`, `src/store.rs` | Apply with the head locked; revisions and gateway replica reports in Postgres; the feed bodies |
| `src/feed.rs` | The head readers long-poll |
| `src/fixtures.rs`, `fixtures/` | The fixtures, below |
| `src/docgen.rs` | Test: `docs/reference/config/` field tables match the schema |

## Fixtures

Other services' tests read these, so the control plane's tests keep them fresh: a stale derived file fails `cargo test`, and `UPDATE_FIXTURES=1 cargo test` rewrites every one.

| Path | |
|------|--|
| `fixtures/catalog/plat5.yml` | A config three kinds deep (organization, project, environment) with the example role set and identity's and audit's resources at their defaults |
| `fixtures/catalog/gateway-feed.json` | Derived: `GET /internal/gateway` for it, every route expanded. The gateway's, identity's and audit's tests read it |
| `fixtures/catalog/identity-feed.json` | Derived: `GET /internal/identity` for it |
| `fixtures/plans/<case>/before.yml`, `after.yml` | The applied config (absent before the first apply) and the one planned |
| `fixtures/plans/<case>/counts.json` | What identity answers for the plan's counts, when it asks |
| `fixtures/plans/<case>/plan.json`, `gateway-feed.json` | Derived: the plan, and the gateway feed for `after.yml` |
| `fixtures/plans/<case>/refused.json` | Derived, instead of the two above: the rule errors `after.yml` is refused with |

## Telemetry

Contract: [`../../docs/reference/telemetry.md`](../../docs/reference/telemetry.md).

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

Health, `/metrics` and the `/internal/*` feed reads are not request-traced; a feed read logs only a failure.

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
UPDATE_FIXTURES=1 cargo test                 # rewrite the derived fixtures
```
