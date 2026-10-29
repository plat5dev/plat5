# End to end

Tests that run against a running Plat5 stack through the gateway. Opt-in: they
skip unless `PLAT5_E2E=1`.

A fresh stack has no config, so the tests apply theirs first:
[`testdata/config.json`](testdata/config.json), the flat config the CLI sends
(`services` as a map), with kinds organization → project → environment, the example
roles, and service accounts turned off at projects.
They dry-run it through the control plane's admin API and apply it with the
plan's revision as `expect`; a plan with no changes applies nothing, so a rerun
leaves the stack as it is. With `AUDIT_ENABLED=false` the config leaves the
audit service out.

The tests serve their own JWKS on host port `5401` (compose's default
`AUTH_JWKS_URI`) and mint tokens with `testdata/jwt-signing-key.pem`, a key
made for these tests and trusted nowhere else. Nothing else may listen on `5401`.

`TestNames` applies a second config, the e2e config with
`users.name.sync: each_sign_in`, and applies the e2e config again when it ends.
`TestRouteChecks` applies the e2e config plus a `probe` service with a project
route and a public route, which route checks match but never call, and applies
the e2e config again when it ends.

`TestEvaluations` (the evaluation API) and `TestUserRoutesKeysInvites` (the
tree and user event feeds) call identity's internal port directly, as a service
on the private network would. [`compose.yml`](compose.yml) publishes it for the
e2e, on loopback port `5404`; a deployment never publishes it.

```bash
# From the repo root, with audit on (the default). A 1s admit cache lets the
# tests see a delete, a restore or a leave at once.
ADMIT_CACHE_TTL=1s ADMIT_NEGATIVE_CACHE_TTL=1s docker compose -f compose/docker-compose.yml -f e2e/compose.yml up -d --build

cd e2e && PLAT5_E2E=1 go test ./... -count=1 -v
```

| Env | Default | |
|-----|---------|--|
| `PLAT5_E2E` | unset | `1` runs the tests |
| `AUDIT_ENABLED` | unset | `false` skips the audit test and leaves audit out of the config, for a stack run with audit off |
| `GATEWAY_URL` | `http://localhost:5400` | The gateway, as clients see it |
| `CONTROL_PLANE_URL` | `http://localhost:5402` | The control plane's admin API |
| `IDENTITY_INTERNAL_URL` | `http://localhost:5404` | Identity's internal port, for the evaluation API and the event feeds |
| `ADMIN_TOKEN` | `dev-admin-token` | Its token |
| `AUTH_ISSUER` | `http://localhost:5401` | Must equal the gateway's `AUTH_ISSUER` |
| `JWKS_ADDR` | `:5401` | Where the tests serve JWKS; the gateway's `AUTH_JWKS_URI` must reach it |
| `E2E_WAIT` | `10m` | How long to wait for the control plane and then the gateway to answer (the dev images build on first start) |

Don't `up --wait`: identity and the gateway wait for the first apply, and the gateway isn't ready until it loads JWKS, which only the running tests serve.
