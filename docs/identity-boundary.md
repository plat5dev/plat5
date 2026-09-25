# Identity Boundary

Authn, organization context, and resource authz are separate layers. Mixing them breaks the platform model.

Every route declares which subject exists: none, the person, or the member-in-org. Those do not mix. Resource permissions are the service’s problem over that subject.

Headers and service rules: [`gateway-contract.md`](gateway-contract.md). Routes: [`routes.md`](routes.md). Errors: [`api-errors.md`](api-errors.md). Identity APIs: [`identity.md`](identity.md).

## Why this cut

The route is a type. A handler does not get extra identity “just in case.” Plat5 authenticates and admits; it will not forward user, role, or IdP claims onto a route that did not declare them.

**Mixed ticket.** A handler that sees `X-User-Id` + org + role invents a different “who is this?” per feature. Billing keys off the user. ACL keys off the org. Admin checks key off role. A member API key shows up and none of it fits. Suspend-member and delete-user diverge. One subject per scope is what stops that.

**User-keyed authz on an org route.** Org-scoped APIs do not receive `X-User-Id`. If your graph is `user:U` on `doc:D`, an org route cannot feed it without a lookup you own — and a user-keyed graph on org-scoped resources crosses orgs unless you add org as a second check everywhere. Key org-scoped policy on `member_id` inside `organization_id`. Person-centric policy belongs on `user` routes.

**Platform role as product ACL.** Identity has no roles. Product permissions are the service’s, over the subject the scope defined. Do not add an owner / admin / member column to fill that gap.

**Identity on `organization` scope.** Identity is the membership store member-resolve calls. Its routes are not member-admission routes. Putting them on `organization` scope would 404 a caller who is not a member and cycle on the store. The path names the subject. The handler does not read a caller header and does not enforce who may call.

Frontend session (cookie, current-org, subdomain) is your client. The request the gateway sees must still name the subject — org in the path for `organization` scope.

Trusting injected headers is a perimeter protocol: [`gateway-contract.md`](gateway-contract.md).

## Layers

| Layer | Question | Owner |
|-------|----------|--------|
| **Authentication** | Who is this? | Gateway + **IdP (JWT)** / identity API keys (credentials stripped before upstream) |
| **Organization context** | Is this credential an **active** member of this organization? | Gateway + **identity** (member resolve or member-scoped key) |
| **API key route scopes** | Does this restricted key share a label with `required_scopes`? | Gateway after admission. Restricted = non-null `scopes` (`[]` or labels). JWTs and `null` skip. Omitted `required_scopes` → any admitted principal. |
| **Resource authorization** | Can this member do X to project/doc/…? | **Business services** — not gateway headers |
| **Who may call identity** | Who may add members, mint keys, delete an org? | The proxy in front of identity. The service refuses illegal states only. |

Route `required_scopes` is a credential intersection: the restricted key’s labels and the route’s labels must overlap.

## Route scopes → subject

| Scope | Subject after gateway | Identity headers |
|-------|----------------------|------------------|
| `public` | None | none |
| `user` | Person (`user_id`) | `X-User-Id` |
| `organization` | Member-in-org | `X-Organization-Id` + `X-Member-Id` only |

One subject per scope. Do not put `X-User-Id` on `organization` routes.

Always: `X-Request-ID`, `traceparent`. The edge may record `user.id` / `organization.id` / `member.id` on spans for ops — that is not the app contract. Org-scoped services do not receive `X-User-Id`.

## No roles

Identity has no `member` / `admin` / `owner` column, response field, or hook. Who may call is not a role. Do not add the column back.

Business services on `organization` scope get `organization_id` + `member_id` only. Resource permissions are the service’s problem.

## Who uses which scope

| Routes | Scope | Why |
|--------|-------|-----|
| User-centric business APIs | **`user`** | Subject is the person (`X-User-Id`). |
| **identity** | not `organization` | Path names the subject (`/users/{user_id}`, `/organizations/{organization_id}`, `/members/{member_id}`). Must not sit behind member-resolve. The handler does not read a caller header. |
| Business APIs under an org path | **`organization`** | Gateway admits active member; service trusts org headers and enforces resource authz. |

**Default on `organization` scope:** trust gateway admission — do not re-check “is this member in the org?” Enforce **resource** authz in the service. Re-checking admission is optional defense-in-depth, not required.

### Credentials on `organization` scope

| Credential | Admission |
|------------|-----------|
| User JWT | Authn → `user_id` → **member resolve** `(user_id, organization_id)` → inject org headers |
| User API key | Validate → `user_id` (+ `scopes`) → same resolve → inject → `required_scopes` if the key is restricted |
| Member API key | Validate → `member_id` + `organization_id` + `scopes` → path org must match + member active → inject (no resolve call) → `required_scopes` if restricted |

### Org-scoped API

```
GET /api/organizations/{organization_id}/projects
Authorization: user JWT or user API key
  (or X-API-Key: member-scoped key for automation)

Gateway: authn → admit active member → inject X-Organization-Id + X-Member-Id → proxy
Service: trust those headers; enforce resource authz as needed
```

No organization-scoped token exchange required for the default path.

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
| Bad or missing credential | **401** `UNAUTHORIZED` |
| Restricted API key missing route `required_scopes` | **403** `FORBIDDEN` |
| Non-member, unknown org, or member not `active` (org-context) | **404** `NOT_FOUND` |
| Admitted route or failed-auth IP over limit | **429** `RATE_LIMITED` |
| Member resolve / key validate down or timeout; Valkey down on a limited request; JWKS unavailable | **503** `SERVICE_UNAVAILABLE` |
| Missing expected identity headers on a protected route (downstream) | **500** `INTERNAL_ERROR` (platform bug) |

Identity **404**s an unknown id, a removed member, and a key or service account addressed under the wrong parent. It does not **404** "not a member." Gateway org-context still **404**s non-member, unknown org, or inactive member.

## Missing headers

| Scope | Expected headers | If missing |
|-------|------------------|------------|
| `user` | `X-User-Id` | `INTERNAL_ERROR` |
| `organization` | `X-Organization-Id`, `X-Member-Id` | `INTERNAL_ERROR` |
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
