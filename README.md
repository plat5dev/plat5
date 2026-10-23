# Plat5

Open-source **platform runtime** for multi-tenant APIs. You declare the tree's kinds in config (organization, project, …); Plat5 owns the nodes, who can reach each one, and the check at the gateway. A role bound at an org reaches every project under it; a role bound at one project reaches only that project. Services get node ids and the caller's member id, never a person's id, and do no auth themselves.

Point the gateway at any OIDC IdP ([`docs/idp-contract.md`](docs/idp-contract.md)), including Plat5 Auth if you run it separately.

The model: [`docs/model.md`](docs/model.md).

## Try it

Consumer apps: install the [CLI](https://github.com/plat5dev/cli) and `plat5 init` / `plat5 start` (pulls GHCR images).

This tree (compose only):

```bash
cd compose
docker compose up --build
```

| Service | URL | Notes |
|---------|-----|--------|
| Gateway | `http://localhost:5400` | API edge; internal metrics on `:8000` |
| Control plane | `http://localhost:5402` | Admin API for kinds, roles, routes, CORS origins (Bearer `ADMIN_TOKEN`, default `dev-admin-token`) |

JWT: set `AUTH_ISSUER`, `AUTH_JWKS_URI`, `AUTH_SUBJECT_CLAIM` (see compose defaults). API keys are an alternative credential; the IdP is still required.

## Layout

| Path | Purpose |
|------|---------|
| `services/gateway/` | Reverse proxy, auth, routing (Rust / Pingora) |
| `services/control-plane/` | The deployment's kinds, roles, routes, and CORS origins: plans, admin API, revisions, feeds (Rust) |
| `services/identity/` | Nodes, people, service accounts, members, bindings, keys, admit (Go) |
| `services/audit/` | Audit log store and read API, per root and by node (Go) |
| `compose/` | Self-contained Plat5 stack |
| `docs/` | Contracts |

## Documentation

| Doc | Contents |
|-----|----------|
| [`docs/model.md`](docs/model.md) | **Start here.** Concepts, rules, what Plat5 won't do |
| [`AGENTS.md`](AGENTS.md) | How to work in this repo (for agents) |
| [`docs/README.md`](docs/README.md) | Contract index |
| [`docs/self-hosting.md`](docs/self-hosting.md) | Production: images, TLS, attach an app |
| [`docs/idp-contract.md`](docs/idp-contract.md) | BYO IdP, subject and name claims |
| [`docs/gateway-contract.md`](docs/gateway-contract.md) | Pipeline, admit, headers to services, perimeter |
| [`docs/routes.md`](docs/routes.md) | Route config format |
| [`docs/control-plane.md`](docs/control-plane.md) | Kinds, plans, and applying documents via the admin API |
| [`docs/identity.md`](docs/identity.md) | Nodes, members, bindings, the generated catalog, admit |
| [`docs/roles.md`](docs/roles.md) | Roles, bindings, and how labels resolve |
| [`docs/audit.md`](docs/audit.md) | Audit log: what the gateway records and how it's read by node |
| [`docs/api-errors.md`](docs/api-errors.md) | Error envelope |
| [`docs/telemetry.md`](docs/telemetry.md) | Logs, traces, metrics |

## Attach a service

1. Implement against Plat5 contracts (reference apps: [`plat5dev/template-*`](https://github.com/orgs/plat5dev/repositories)).
2. Set `url` in `routes.yml` to an address the **gateway** can reach.
3. Add its routes file to your config directory and apply the whole config (shows the plan first):

```bash
plat5 apply ./plat5
```

4. Trust the path and `X-Plat5-*` headers the gateway wrote; do not validate JWTs or decide access in the service. They're authentic only if nothing but a front door can reach the app ([`docs/gateway-contract.md`](docs/gateway-contract.md)).

## Telemetry

Stdout logs + `/metrics` always. OTLP opt-in (traces and metrics both on when endpoint set). [`docs/telemetry.md`](docs/telemetry.md).

## License

Apache-2.0 — see [LICENSE](LICENSE).
