# Plat5 contracts

**Start here:** [`identity-boundary.md`](identity-boundary.md) — which subject exists on a request, why the layers do not mix, what you own vs what Plat5 owns.

The rest of this directory is the wire: subject, routes, errors, identity APIs. The boundary is the model; the other pages are how it shows up on the network. They should agree.

Locked product invariants (what not to grow into): [`../AGENTS.md`](../AGENTS.md).

Operator path (images + compose, not the CLI): [`self-hosting.md`](self-hosting.md).

Per-service details live in each service's `README.md`.

## Contracts

| Document | Purpose |
|----------|---------|
| [`api-errors.md`](api-errors.md) | Error response envelope and error codes |
| [`lists.md`](lists.md) | Identity list pagination; optional shape for business APIs |
| [`error-copy.md`](error-copy.md) | Stripe-style `message` — product copy appendix |
| [`container-labels.md`](container-labels.md) | Docker labels (optional collector scrape) |
| [`health-checks.md`](health-checks.md) | Health check endpoints |
| [`naming-conventions.md`](naming-conventions.md) | Service names, terminology, paths, subject |
| [`telemetry.md`](telemetry.md) | Logs, traces, metrics (stdout, scrape, OTLP) |

## Identity and edge

| Document | Purpose |
|----------|---------|
| [`identity-boundary.md`](identity-boundary.md) | **Start here.** Subjects, layers, what not to mix |
| [`gateway-contract.md`](gateway-contract.md) | Auth delegation, subject fill, perimeter, TLS, service rules |
| [`idp-contract.md`](idp-contract.md) | BYO IdP, JWKS, user-id claim mapping |
| [`routes.md`](routes.md) | Route publishing, scopes, `route_prefix`, rate-limit policies |
| [`route-registry.md`](route-registry.md) | Desired state (Postgres) + etcd projection |
| [`identity.md`](identity.md) | Identity service: orgs, members, service accounts, API keys, sessions, internal validate |
