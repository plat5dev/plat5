# IdP contract

Plat5's gateway accepts signed JWT access tokens from any issuer (OAuth 2.0 JWT access tokens, RFC 9068). An OIDC provider qualifies because it issues signed JWTs; OIDC discovery, ID tokens, and userinfo endpoints are never required.

## Gateway requirements

| Requirement | Gateway env |
|-------------|-------------|
| Issuer URL (`iss`) | `AUTH_ISSUER` (required) |
| JWKS | `AUTH_JWKS_URI` (required) |
| Allowed audiences (`aud`) | `AUTH_ALLOWED_AUDIENCES` (required; comma-separated, or `*` to accept any) |
| Subject claim path | `AUTH_SUBJECT_CLAIM` (dotted JSON path, default `sub`) |
| Name claim paths | `AUTH_NAME_CLAIMS` (comma-separated dotted JSON paths, default `name,preferred_username`) |

Each token is checked for `iss`, `aud`, `exp`, a `kid` header matching a key in JWKS, signature validity, and a non-empty subject claim.

## Subject claim

`AUTH_SUBJECT_CLAIM` names the claim holding the person's stable subject at the IdP. The gateway sends `(iss, subject)` to identity's admit; a new pair creates the person. Services never see the IdP's subject — they see a per-tree member id on tree routes, and a user id on user routes.

- Most IdPs (Auth0, Okta, Entra ID, Keycloak, Cognito) put a stable subject in `sub` (the default).
- Issuers that place the stable id in a nested claim use a dotted path (for example, `properties.user_id` for OpenAuth / [Plat5 Auth](https://github.com/plat5dev/auth)).

Missing or empty subject claim → **401**.

## A person's name

A person has one display name across every tree, shown in member lookups and `GET /user`. It is `null` until known.

- **Seeded on first sign-in:** On admit, the gateway reads the claims in `AUTH_NAME_CLAIMS` in order and sends identity the first non-empty value (trimmed, truncated to 128 chars). Identity stores it only if the person's name is currently empty.
- **Editable in Plat5:** Once created, the person can set or clear their name with `PATCH /user { name }` (and an operator can with `PATCH /users/{user_id} { name }`). Clearing it to `null` lets a later sign-in with a claim seed it again.

An org admin cannot edit a person's name, because it applies across every tree. Service accounts have their own names, managed by their tree.

## Bring your own IdP

1. Set `AUTH_ISSUER` and `AUTH_JWKS_URI` (a URL reachable from the gateway container).
2. Set `AUTH_ALLOWED_AUDIENCES` to your API audience(s), or `*`.
3. Set `AUTH_SUBJECT_CLAIM` if your IdP's stable subject isn't `sub`, and `AUTH_NAME_CLAIMS` if your access tokens carry a name under a different claim.

Requests may also authenticate with API keys on the same `Authorization: Bearer` header. The gateway still requires `AUTH_ISSUER`, `AUTH_JWKS_URI`, and `AUTH_ALLOWED_AUDIENCES` to boot.
