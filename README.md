# Plat5

Open-source **platform runtime**. Authenticate at the gateway, delegate identity via headers, register routes, manage API keys and organizations.

Login UI / user directory is **not included** — point the gateway at any OIDC IdP ([`docs/idp-contract.md`](docs/idp-contract.md)), including Plat5 Auth if you run it separately.

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

JWT: set `AUTH_ISSUER`, `AUTH_JWKS_URI`, `AUTH_USER_ID_CLAIM` (see compose defaults). API keys work without an IdP.

## Layout

| Path | Purpose |
|------|---------|
| `services/gateway/` | Reverse proxy, auth, routing (Rust / Pingora) |
| `services/route-registry/` | Route admin API → etcd (Rust) |
| `services/api-keys/` | API key CRUD + validate (Go) |
| `services/organizations/` | Orgs, memberships, resolve (Go) |
| `compose/` | Self-contained Plat5 stack |
| `docs/` | Contracts |

## Documentation

| Doc | Contents |
|-----|----------|
| [`docs/README.md`](docs/README.md) | Contract index |
| [`docs/idp-contract.md`](docs/idp-contract.md) | BYO IdP / JWT user-id claim |
| [`docs/gateway-contract.md`](docs/gateway-contract.md) | Auth delegation, identity headers |
| [`docs/routes.md`](docs/routes.md) | Route config format |
| [`docs/route-registry.md`](docs/route-registry.md) | Apply routes via admin API |
| [`docs/identity-boundary.md`](docs/identity-boundary.md) | Authn vs org context vs resource authz |
| [`docs/organizations.md`](docs/organizations.md) | Organizations API |
| [`docs/api-errors.md`](docs/api-errors.md) | Error envelope |
| [`docs/telemetry.md`](docs/telemetry.md) | Logs, traces, metrics |

## Attach a service

1. Implement against Plat5 contracts (reference apps: [`plat5dev/template-*`](https://github.com/orgs/plat5dev/repositories)).
2. Set `url` in `routes.yml` to an address the **gateway** can reach.
3. Apply routes:

```bash
curl -sS -X POST http://localhost:5002/v1/apply \
  -H "Authorization: Bearer dev-admin-token" \
  -H "Content-Type: application/yaml" \
  --data-binary @routes.yml
```

4. Trust gateway identity headers; do not validate JWTs in the service.

## Telemetry

Stdout logs + `/metrics` always. OTLP opt-in (traces by default when endpoint set; metrics OTLP explicit). [`docs/telemetry.md`](docs/telemetry.md).

## License

Apache-2.0 — see [LICENSE](LICENSE).
