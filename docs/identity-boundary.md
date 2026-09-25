# Identity Boundary

Authn, organization context, and resource authz are separate layers. Mixing them breaks the platform model.

Every route declares which subject exists: none, the person, the org, or the member in that org. Those do not mix. Resource permissions are the service’s problem over that subject.

Headers and service rules: [`gateway-contract.md`](gateway-contract.md). Routes: [`routes.md`](routes.md). Errors: [`api-errors.md`](api-errors.md). Identity APIs: [`identity.md`](identity.md).

## Why this cut

The route is a type. A handler does not get extra identity “just in case.” Plat5 authenticates and admits; it will not forward user, role, or IdP claims onto a route that did not declare them.

**Mixed ticket.** A handler that sees `X-User-Id` + org + role invents a different “who is this?” per feature. Billing keys off the user. ACL keys off the org. Admin checks key off role. A member API key shows up and none of it fits. Suspend-member and delete-user diverge. One subject per scope is what stops that.

**User-keyed authz on an org route.** `organization` and `member` routes do not receive `X-User-Id`. If your graph is `user:U` on `doc:D`, those routes cannot feed it without a lookup you own. Person-centric policy belongs on `user` routes. Policy that needs the member belongs on `member` scope. `organization` scope does not receive `member_id`.

**Platform role as product ACL.** Identity has no roles. Product permissions are the service’s, over the subject the scope defined. Do not add an owner / admin / member column to fill that gap.

**User-subject routes are not org admission.** A route whose subject is `user_id` stays on `user` scope. Identity's organization and member routes are published on those scopes because the template fills the subject from the credential. The handler does not read a caller header and does not enforce who may call. Putting a user-subject route on `organization` or `member` would drop `user_id`.

Frontend session (cookie, current-org, subdomain) is your client. The client does not supply a subject id. The credential does.

Trusting injected headers is a perimeter protocol: [`gateway-contract.md`](gateway-contract.md).

## Layers

| Layer | Question | Owner |
|-------|----------|--------|
| **Authentication** | Who is this? | Gateway + **IdP (JWT)** / identity keys and sessions (credentials stripped before upstream) |
| **Scope projection** | Which fields is this route allowed to see? | Gateway. The credential is the proof. The scope drops fields. |
| **API key route scopes** | Does this restricted key share a label with `required_scopes`? | Gateway after admission. Restricted = non-null `scopes` (`[]` or labels). JWTs and `null` skip. Omitted `required_scopes` → any admitted principal. |
| **Resource authorization** | Can this member do X to project/doc/…? | **Business services** — not gateway headers |
| **Who may call identity** | Who may add members, mint keys, delete an org? | The proxy in front of identity. The service refuses illegal states only. |

Route `required_scopes` is a credential intersection: the restricted key’s labels and the route’s labels must overlap.

## Route scopes → subject

| Scope | Subject after gateway | Identity headers |
|-------|----------------------|------------------|
| `public` | None | none |
| `user` | Person (`user_id`) | `X-User-Id` |
| `organization` | The org | `X-Organization-Id` only |
| `member` | The member in that org | `X-Organization-Id` + `X-Member-Id` |

One subject per scope. Do not put `X-User-Id` on `organization` or `member` routes. Do not put `X-Member-Id` on `organization` routes.

Always: `X-Request-ID`, `traceparent`. The edge may record dropped ids on spans for ops — that is not the app contract.

## No roles

Identity has no `member` / `admin` / `owner` column, response field, or hook. Who may call is not a role. Do not add the column back.

Business services on `organization` scope get `organization_id` only. `member` scope gets `organization_id` and `member_id`. Resource permissions are the service’s problem.

## Who uses which scope

| Routes | Scope | Why |
|--------|-------|-----|
| User-centric business APIs, and identity routes whose subject is the person | **`user`** | Subject is the person (`X-User-Id`). |
| Org-scoped business APIs, and identity routes whose subject is the org | **`organization`** | Credential is a member of that org. Handler sees `X-Organization-Id` only. |
| Routes whose subject is the member | **`member`** | Same credential. Handler sees `X-Organization-Id` and `X-Member-Id`. |

**Default:** trust gateway admission. Do not re-check “is this member in the org?” Enforce **resource** authz in the service.

### Credentials

| Scope | Credential |
|-------|------------|
| `user` | User JWT or user API key |
| `organization`, `member` | Member API key or member session |

Wrong credential for the scope is **401**. A user JWT does not become an org subject.

### Org-scoped API

```
GET /api/projects
X-API-Key: member key or member session

Gateway: admit → inject X-Organization-Id → proxy
Service: trust that header; enforce resource authz as needed
```

If the handler needs `member_id`, the route is `member` scope.

### User-scoped API

```
GET /api/user/widgets
Authorization: user JWT or user API key

Gateway: authn → inject X-User-Id → proxy
Service: subject is the person
```

### Identity

```
GET /users/{user_id}/memberships
POST /organizations/{organization_id}/members
PATCH /members/{member_id}
```

The path names every id the handler reads. There is no caller header. Who may call is the proxy. Identity refuses illegal states: slug uniqueness, one membership row per user per org, last member, the invite machine, and an address that does not exist.

## Error split (locked)

| Case | HTTP / code |
|------|-------------|
| Bad, missing, or wrong credential for the scope | **401** `UNAUTHORIZED` |
| Restricted API key missing route `required_scopes` | **403** `FORBIDDEN` |
| Unknown id (identity handlers) | **404** `NOT_FOUND` |
| Admitted route or failed-auth IP over limit | **429** `RATE_LIMITED` |
| Key or session validate down or timeout; Valkey down on a limited request; JWKS unavailable | **503** `SERVICE_UNAVAILABLE` |
| Missing expected identity headers on a protected route (downstream) | **500** `INTERNAL_ERROR` (platform bug) |
| Path param is not one segment | **400** `INVALID_REQUEST` |
| Subject id is not one segment | **500** `INTERNAL_ERROR` |

Identity **404**s an unknown id, a removed member, and a key or service account addressed under the wrong parent. It does not **404** "not a member." The gateway does not **404** a wrong credential. Inactive, expired, and unknown credentials are **401**.

## Missing headers

| Scope | Expected headers | If missing |
|-------|------------------|------------|
| `user` | `X-User-Id` | `INTERNAL_ERROR` |
| `organization` | `X-Organization-Id` | `INTERNAL_ERROR` |
| `member` | `X-Organization-Id`, `X-Member-Id` | `INTERNAL_ERROR` |
| `public` | none | — |

Do not return `UNAUTHORIZED` for missing identity headers — the gateway already authenticated (or should have rejected) the client.

## Invites

Org invites live in **identity** (`organization_invites`). Create, list, and revoke are `/organizations/{organization_id}/invites`. List includes `token` while `active`. Redeem is `POST /users/{user_id}/invites/redeem`. The host sends any email. Unknown token → 404. Redeemed / revoked / expired → 409 `CONFLICT` (`{ field: "status", value }`).

## What is not Plat5 identity (here)

- Project / document / generic resource ACL in the gateway
- FGA / ReBAC engines
- Tenant as a name for an organization
- Operator / employee admin planes
- A role column. Identity has no `member` / `admin` / `owner`. Do not add one.
- Service accounts as a parallel auth system (they are members with keys)
- Multi-org service accounts
- SMTP in identity (invites return a token; the console sends mail if it wants)
- Pending member rows (invite redeem inserts an **active** member; add-by-`user_id` remains)
