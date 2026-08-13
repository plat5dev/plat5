# Plat5 contracts

Contracts, standards, and conventions for Plat5. Follow them for gateway auth delegation, dynamic routing, unified error handling, and optional telemetry.

Locked product invariants (what not to grow into): [`../AGENTS.md`](../AGENTS.md).

Per-service details live in each service's `README.md`.

## Contracts

| Document | Purpose |
|----------|---------|
| [`api-errors.md`](api-errors.md) | Error response envelope and error codes |
| [`container-labels.md`](container-labels.md) | Docker labels (optional collector scrape) |
| [`health-checks.md`](health-checks.md) | Health check endpoints |
| [`naming-conventions.md`](naming-conventions.md) | Service names, terminology, paths, headers |
| [`telemetry.md`](telemetry.md) | Logs, traces, metrics (stdout, scrape, OTLP) |

## Identity and edge

| Document | Purpose |
|----------|---------|
| [`gateway-contract.md`](gateway-contract.md) | Auth delegation, scope headers, TLS, service rules |
| [`idp-contract.md`](idp-contract.md) | BYO IdP, JWKS, user-id claim mapping |
| [`routes.md`](routes.md) | Route publishing, scopes, `route_prefix` |
| [`route-registry.md`](route-registry.md) | Desired state (Postgres) + etcd projection |
| [`identity-boundary.md`](identity-boundary.md) | Authn vs organization context vs resource authz |
| [`identity.md`](identity.md) | Identity service: orgs, members, service accounts, API keys, internal validate/resolve |
