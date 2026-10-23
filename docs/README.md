# Plat5 contracts

`docs/` is law. **Start here:** [`model.md`](model.md): concepts, the rules that hold everywhere, where each decision lives, and what Plat5 won't do. The rest of this directory is how the model shows up on the wire. When a page disagrees with `model.md`, the page is wrong.

Why each decision was made, with references: [`decisions.md`](decisions.md). That's a record, not a contract.

Operator path (images + compose, not the CLI): [`self-hosting.md`](self-hosting.md). Per-service details live in each service's `README.md`.

## Model and edge

| Document | Purpose |
|----------|---------|
| [`model.md`](model.md) | **Start here.** Concepts, principles, rules, what's not in Plat5, extension points |
| [`control-plane.md`](control-plane.md) | Kinds, the plan, validation across documents, the admin API and feeds |
| [`routes.md`](routes.md) | Route config: scopes, chain params, `upstream`, labels, rate limits, audit |
| [`roles.md`](roles.md) | Roles, bindings, resolution, who may assign |
| [`gateway-contract.md`](gateway-contract.md) | The pipeline, admit, headers to services, rules for services, perimeter |
| [`identity.md`](identity.md) | Nodes, members, bindings, service accounts, keys, invites, the generated catalog, admit, lifecycle events |
| [`idp-contract.md`](idp-contract.md) | BYO IdP, subject and name claims |
| [`audit.md`](audit.md) | Audit log per root: what's recorded, delivery, details headers, reads by node |

## Conventions

| Document | Purpose |
|----------|---------|
| [`api-errors.md`](api-errors.md) | Error envelope and codes |
| [`error-copy.md`](error-copy.md) | Stripe-style `message`: product copy |
| [`lists.md`](lists.md) | List pagination |
| [`naming-conventions.md`](naming-conventions.md) | Terms, paths, headers, fields |
| [`health-checks.md`](health-checks.md) | Health check endpoints |
| [`telemetry.md`](telemetry.md) | Logs, traces, metrics (stdout, scrape, OTLP) |
| [`container-labels.md`](container-labels.md) | Docker labels (optional collector scrape) |

## Transitional

| Document | Purpose |
|----------|---------|
| [`v2-cutover.md`](v2-cutover.md) | What changes from v1, what gets removed, build order. Deleted once v2 ships |
