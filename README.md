# Plat5

Open-source **platform runtime**. Every route declares which subject exists — none, the person, or the member-in-org. Those do not mix. The gateway authenticates and admits; services get exactly that subject and own resource permissions.

Point the gateway at any OIDC IdP ([`docs/idp-contract.md`](docs/idp-contract.md)), including Plat5 Auth if you run it separately. User-scope APIs (memberships, user keys, identity itself) are `user` scope.

The model: [`docs/identity-boundary.md`](docs/identity-boundary.md).

## Try it

Consumer apps: install the [CLI](https://github.com/plat5dev/cli) and `plat5 init` / `plat5 start` (pulls GHCR images).

This tree (compose only):

```bash
cd compose
docker compose up --build
```

| Service | URL | Notes |
|---------|-----|--------|
| Gateway | `http://localhost:5001` | API edge; internal metrics on `:8000` |
| Route registry | `http://localhost:5002` | Admin API (Bearer `ADMIN_TOKEN`, default `dev-admin-token`) |

JWT: set `AUTH_ISSUER`, `AUTH_JWKS_URI`, `AUTH_USER_ID_CLAIM` (see compose defaults). API keys are an alternative credential; the IdP is still required.

## Layout

| Path | Purpose |
|------|---------|
| `services/gateway/` | Reverse proxy, auth, routing (Rust / Pingora) |
| `services/route-registry/` | Route admin API → etcd (Rust) |
| `services/identity/` | Identity control plane (Go) |
| `compose/` | Self-contained Plat5 stack |
| `docs/` | Contracts |

## Documentation

| Doc | Contents |
|-----|----------|
| [`docs/identity-boundary.md`](docs/identity-boundary.md) | **Start here.** Subjects, layers, what not to mix |
| [`AGENTS.md`](AGENTS.md) | Locked invariants and stop conditions (for agents) |
| [`docs/README.md`](docs/README.md) | Contract index |
| [`docs/self-hosting.md`](docs/self-hosting.md) | Production: images, TLS, attach an app |
| [`docs/idp-contract.md`](docs/idp-contract.md) | BYO IdP / JWT user-id claim |
| [`docs/gateway-contract.md`](docs/gateway-contract.md) | Auth delegation, subject fill, perimeter |
| [`docs/routes.md`](docs/routes.md) | Route config format |
| [`docs/route-registry.md`](docs/route-registry.md) | Apply routes via admin API |
| [`docs/identity.md`](docs/identity.md) | Identity service API |
| [`docs/api-errors.md`](docs/api-errors.md) | Error envelope |
| [`docs/telemetry.md`](docs/telemetry.md) | Logs, traces, metrics |

## Attach a service

1. Implement against Plat5 contracts (reference apps: [`plat5dev/template-*`](https://github.com/orgs/plat5dev/repositories)).
2. Set `url` in `routes.yml` to an address the **gateway** can reach.
3. Apply routes:

```bash
curl -sS -X POST http://localhost:5002/apply \
  -H "Authorization: Bearer dev-admin-token" \
  -H "Content-Type: application/yaml" \
  --data-binary @routes.yml
```

4. Trust the path the gateway wrote; do not validate JWTs in the service. That path is authentic only if nothing else can reach the app — [`docs/gateway-contract.md`](docs/gateway-contract.md).

## Telemetry

Stdout logs + `/metrics` always. OTLP opt-in (traces and metrics both on when endpoint set). [`docs/telemetry.md`](docs/telemetry.md).

## License

Apache-2.0 — see [LICENSE](LICENSE).
