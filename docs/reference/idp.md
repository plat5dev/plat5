# IdP contract

Plat5's gateway validates JWTs from **any** OIDC-compatible issuer.

## Gateway requirements

| Requirement | Config |
|-------------|--------|
| Issuer URL (`iss`) | `AUTH_ISSUER` |
| JWKS | `AUTH_JWKS_URI` |
| Allowed audiences | `AUTH_ALLOWED_AUDIENCES`, required (comma-separated; `*` accepts any audience) |
| Subject claim path | `AUTH_SUBJECT_CLAIM` (dotted JSON path). Default `sub` |
| Name claim | `users.name_claim` in `plat5.yml` ([settings](config/settings.md)). Default `name` |

Also required on tokens: signature via JWKS, `exp`, and a `kid` in the JWT header.

## Subject claim

`AUTH_SUBJECT_CLAIM` names the claim that holds the person's subject at the IdP. The gateway reads it and the token's issuer (`iss`), and admit resolves the pair to the person ([`reference/api/internal.md`](api/internal.md#admit)). A new subject creates the person. Services never see the IdP's subject: they see a member id per tree, and the user id on user routes.

| `AUTH_SUBJECT_CLAIM` | Typical IdP |
|----------------------|-------------|
| `sub` (the default) | Auth0, many standard OIDC providers |
| `properties.user_id` | Nested custom subject properties (e.g. OpenAuth-style, Plat5 Auth) |
| `user_id` | Flat custom claim |

Local compose defaults use `properties.user_id` and a host-published JWKS URL on port `5401`; override for your IdP.

Missing or empty claim → **401** (same as invalid token).

A person is linked to their IdP subjects, and services hold member and user ids, never subjects. There's no API yet to link a second subject ([`TODO.md`](../../TODO.md)), so switching IdPs today means rewriting `idp_links` by hand; done that way, every member and user id services hold stays the same.

## Name claim

The claim named by `users.name_claim` sets the person's name, shown by member lookups and `GET /user`. It's read on each admit that reaches identity, so a name change at the IdP shows up within the gateway's cache TTL. A missing or empty claim leaves the name as it was. Plat5 has no other way to set a person's name.

## Bring your own IdP

1. Point `AUTH_ISSUER` + `AUTH_JWKS_URI` at your provider (public URL or host-published port).
2. Set `AUTH_SUBJECT_CLAIM` if your IdP's subject isn't `sub`, and `users.name_claim` if it doesn't use `name`.
3. Set `AUTH_ALLOWED_AUDIENCES` to your API audience(s), or `*` to accept any.
4. Plat5 doesn't share a Docker network with the IdP. Reach JWKS via a host or public URL (`host.docker.internal` from containers when the IdP publishes on the host).

A request may use an API key instead of a JWT, on the same `Authorization: Bearer` header. The gateway still requires a configured IdP (`AUTH_ISSUER`, `AUTH_JWKS_URI`) to start and become ready.
