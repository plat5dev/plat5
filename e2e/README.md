# End to end

Tests that run against a running Plat5 stack through the gateway. Opt-in: they
skip unless `PLAT5_E2E=1`.

The tests serve their own JWKS on host port `5401` (compose's default
`AUTH_JWKS_URI`) and mint tokens with `testdata/jwt-signing-key.pem`, a key
made for these tests and trusted nowhere else. Nothing else may listen on `5401`.

```bash
# From the repo root, with audit on (the default). A 1s admit cache lets the
# test see a delete at once.
APIKEY_CACHE_TTL_SECS=1 docker compose -f compose/docker-compose.yml up -d --build

cd e2e && PLAT5_E2E=1 go test ./... -count=1 -v
```

| Env | Default | |
|-----|---------|--|
| `PLAT5_E2E` | unset | `1` runs the tests |
| `AUDIT_ENABLED` | unset | `false` skips the audit test, for a stack run with audit off |
| `GATEWAY_URL` | `http://localhost:5400` | The gateway, as clients see it |
| `AUTH_ISSUER` | `http://localhost:5401` | Must equal the gateway's `AUTH_ISSUER` |
| `JWKS_ADDR` | `:5401` | Where the tests serve JWKS; the gateway's `AUTH_JWKS_URI` must reach it |
| `E2E_WAIT` | `10m` | How long to wait for the gateway to answer (the dev images build on first start) |

Don't `up --wait`: the gateway isn't ready until it loads JWKS, which only the running tests serve.
