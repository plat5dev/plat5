# IdP contract

Plat5’s gateway validates JWTs from **any** OIDC-compatible issuer.

## Gateway requirements

| Requirement | Config |
|-------------|--------|
| Issuer URL (`iss`) | `AUTH_ISSUER` |
| JWKS | `AUTH_JWKS_URI` |
| Allowed audiences | `AUTH_ALLOWED_AUDIENCES` (comma-separated; empty = skip `aud` check) |
| Subject claim path | `AUTH_USER_ID_CLAIM` (dotted JSON path) |

Also required on tokens: signature via JWKS, `exp`, and a `kid` in the JWT header.

## Subject claim

`AUTH_USER_ID_CLAIM` names the claim that holds the person's subject at the IdP. The gateway reads it and the token's issuer (`iss`), and identity resolves the pair to the person's `user_id` in the directory ([`identity.md`](identity.md#credential-validate)). A new subject creates the person. A `user` route fills `{subject.user_id}`, Plat5's own id, never the IdP's subject.

| `AUTH_USER_ID_CLAIM` | Typical IdP |
|----------------------|-------------|
| `sub` | Auth0, many standard OIDC providers |
| `properties.user_id` | Nested custom subject properties (e.g. OpenAuth-style) |
| `user_id` | Flat custom claim |

Local compose defaults use `properties.user_id` and a host-published JWKS URL on port `5000` — override for your IdP.

Missing or empty claim → **401** (same as invalid token).

Downstream services trust the path the gateway wrote. Identity links a person to one or more IdP subjects, so services never store an IdP's id. Switching IdPs, or linking a second one, changes the links, not the `user_id` services hold.

## Bring your own IdP

1. Point `AUTH_ISSUER` + `AUTH_JWKS_URI` at your provider (public URL or host-published port).
2. Set `AUTH_USER_ID_CLAIM` (often `sub`).
3. Set `AUTH_ALLOWED_AUDIENCES` to your API audience(s) if the IdP sets `aud`. Default Plat5 API audience is often `plat5`.
4. Plat5 does not share a Docker network with the IdP. Reach JWKS via host/public URL (`host.docker.internal` from containers when the IdP publishes on the host).

A request may use an API key instead of a JWT. The gateway still requires a configured IdP (`AUTH_ISSUER`, `AUTH_JWKS_URI`) to start and become ready.
