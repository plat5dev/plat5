# IdP contract

Plat5’s gateway validates JWTs from **any** OIDC-compatible issuer. Plat5 does not ship a login UI or user directory.

## Gateway requirements

| Requirement | Config |
|-------------|--------|
| Issuer URL (`iss`) | `AUTH_ISSUER` |
| JWKS | `AUTH_JWKS_URI` |
| Allowed audiences | `AUTH_ALLOWED_AUDIENCES` (comma-separated; empty = skip `aud` check) |
| User id claim path | `AUTH_USER_ID_CLAIM` (dotted JSON path) |

Also required on tokens: signature via JWKS, `exp`, and a `kid` in the JWT header.

## User id claim

Plat5 identity (`X-User-Id`, identity service keys/members) uses an **opaque string** user id. The gateway reads it from the JWT via `AUTH_USER_ID_CLAIM`.

| `AUTH_USER_ID_CLAIM` | Typical IdP |
|----------------------|-------------|
| `sub` | Auth0, many standard OIDC providers |
| `properties.user_id` | Nested custom subject properties (e.g. OpenAuth-style) |
| `user_id` | Flat custom claim |

Local compose defaults use `properties.user_id` and a host-published JWKS URL on port `5000` — override for your IdP.

Missing or empty claim → **401** (same as invalid token).

Downstream services never validate JWTs; they trust gateway headers. There is **no FK** from identity data to an IdP user table — stable ids are an operator concern when switching IdPs.

## Bring your own IdP

1. Point `AUTH_ISSUER` + `AUTH_JWKS_URI` at your provider (public URL or host-published port).
2. Set `AUTH_USER_ID_CLAIM` (often `sub`).
3. Set `AUTH_ALLOWED_AUDIENCES` to your API audience(s) if the IdP sets `aud`. Default Plat5 API audience is often `plat5`.
4. Plat5 does not share a Docker network with the IdP. Reach JWKS via host/public URL (`host.docker.internal` from containers when the IdP publishes on the host).

A request may use an API key instead of a JWT. The gateway still requires a configured IdP (`AUTH_ISSUER`, `AUTH_JWKS_URI`) to start and become ready.
