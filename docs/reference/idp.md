# IdP contract

Plat5's gateway accepts signed JWT access tokens from any issuer: OAuth 2.0 access tokens in the JWT shape of RFC 9068. An OpenID Connect provider qualifies because it issues such tokens; OpenID Connect itself is never required. The gateway reads only the access token: no ID token, no userinfo call, no discovery document.

## Gateway requirements

| Requirement | Config |
|-------------|--------|
| Issuer URL (`iss`) | `AUTH_ISSUER` |
| JWKS | `AUTH_JWKS_URI` |
| Allowed audiences | `AUTH_ALLOWED_AUDIENCES`, required (comma-separated; `*` accepts any audience) |
| Subject claim path | `AUTH_SUBJECT_CLAIM` (dotted JSON path). Default `sub` |
| Name claims | `users.name.claims` in `plat5.yml` ([settings](config/settings.md)). Default `[name, preferred_username]` |

Each token is checked for `iss` against `AUTH_ISSUER`, `aud` against the allowed audiences, `exp`, a `kid` in the JWT header, its signature against the JWKS key with that `kid`, and a non-empty subject claim.

## Subject claim

`AUTH_SUBJECT_CLAIM` names the claim that holds the person's subject at the IdP. The gateway reads it and the token's issuer (`iss`), and admit resolves the pair to the person ([`reference/api/internal.md`](api/internal.md#admit)). A new subject creates the person. Services never see the IdP's subject: they see a member id per tree, and the user id on user routes.

| `AUTH_SUBJECT_CLAIM` | Typical IdP |
|----------------------|-------------|
| `sub` (the default) | IdPs that put a stable subject in `sub`, JWT's registered subject claim (RFC 7519): Auth0, Okta, Entra ID, Keycloak, Cognito |
| `properties.user_id` | Plat5 Auth. It's built on OpenAuth, whose own `sub` is a hash of the token's properties, so it isn't stable; `properties.user_id` is |
| `user_id` | Flat custom claim |

Local compose defaults use `properties.user_id` and a host-published JWKS URL on port `5401`; override for your IdP.

Missing or empty claim → **401** (same as invalid token).

A person is linked to their IdP subjects, and services hold member and user ids, never subjects. There's no API yet to link a second subject ([`TODO.md`](../../TODO.md)), so switching IdPs today means rewriting `idp_links` by hand; done that way, every member and user id services hold stays the same.

## A person's name

A person has one name across every tree, shown by member lookups and `GET /user`. It's `null` until it's known. Plat5 stores it, and `users.name.sync` decides who owns it: the IdP, or the person and an operator.

**From the token.** On each admit that reaches identity, the gateway reads every claim in `users.name.claims`, in order, from the verified token, and sends identity the first non-empty one, trimmed. `name` and `preferred_username` are profile claims many IdPs leave out of access tokens, so they're a bonus when present; Plat5 Auth's tokens carry no name at all. A claim longer than 128 characters is cut to 128. `users.name.sync` decides what a claim does:

| `sync` | A sign-in with a non-empty claim | Who else sets the name |
|--------|----------------------------------|------------------------|
| `first_sign_in` (the default) | Sets the name only while it's empty. Once it's set, by a claim, the person or an operator, sign-ins never change it | The person and an operator, who can each set or clear it |
| `each_sign_in` | Sets the name every time, so a change at the IdP shows up within the gateway's cache TTL | Nobody: the IdP owns the name. An edit from the person or an operator is **409** `NAME_OWNED_BY_IDP`, and an abusive name is fixed at the IdP |

A sign-in with no name claim, or only empty ones, leaves the name as it is in both modes.

**By the person or an operator,** under `first_sign_in`: the person with `PATCH /user { name }` on the user scope ([generated routes](config/generated-routes.md#identitys-routes)), an operator with `PATCH /users/{user_id} { name }` on identity's service API ([identity's API](api/identity.md#users)). Both are the same edit; identity doesn't ask who sent it. `null` clears the name, so the next sign-in with a claim sets it again.

An org admin never sets it: the name is the person's in every tree, not one tree's. A name is 1 to 128 characters after trimming ([field limits](api/identity.md#field-limits)). A name change isn't a lifecycle event. Service accounts keep their own names, set by their tree.

## Bring your own IdP

1. Point `AUTH_ISSUER` + `AUTH_JWKS_URI` at your provider (public URL or host-published port).
2. Set `AUTH_SUBJECT_CLAIM` if your IdP's subject isn't `sub`. Set `users.name.claims` to where your IdP's access tokens carry a name, if they do, and `users.name.sync` to `each_sign_in` if the IdP should own names.
3. Set `AUTH_ALLOWED_AUDIENCES` to your API audience(s), or `*` to accept any.
4. Plat5 doesn't share a Docker network with the IdP. Reach JWKS via a host or public URL (`host.docker.internal` from containers when the IdP publishes on the host).

A request may use an API key instead of a JWT, on the same `Authorization: Bearer` header. The gateway still requires a configured IdP (`AUTH_ISSUER`, `AUTH_JWKS_URI`) to start and become ready.
