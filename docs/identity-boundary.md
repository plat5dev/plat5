# Identity Boundary

Authn, organization context, and resource authz are separate layers. Mixing them breaks the platform model.

Every route declares which subject exists: none, the person, or the member-in-org. Those do not mix. Resource permissions are the service’s problem over that subject.

Headers and service rules: [`gateway-contract.md`](gateway-contract.md). Routes: [`routes.md`](routes.md). Errors: [`api-errors.md`](api-errors.md). Identity APIs: [`identity.md`](identity.md).

## Why this cut

The route is a type. A handler does not get extra identity “just in case.” Plat5 authenticates and admits; it will not forward user, role, or IdP claims onto a route that did not declare them.

**Mixed ticket.** A handler that sees `X-User-Id` + org + role invents a different “who is this?” per feature. Billing keys off the user. ACL keys off the org. Admin checks key off role. A member API key shows up and none of it fits. Suspend-member and delete-user diverge. One subject per scope is what stops that.

**User-keyed authz on an org route.** Org-scoped APIs do not receive `X-User-Id`. If your graph is `user:U` on `doc:D`, an org route cannot feed it without a lookup you own — and a user-keyed graph on org-scoped resources crosses orgs unless you add org as a second check everywhere. Key org-scoped policy on `member_id` inside `organization_id`. Person-centric policy belongs on `user` routes.

**Platform role as product ACL.** Identity `owner` / `admin` / `member` is for membership administration (add member, rotate SA keys, transfer owner). Product permissions are the service’s, over the subject the scope defined.

**Identity on `organization` scope.** If org administration required gateway member-resolve, identity could not be the authority that defines membership. Memberships and invite redeem are person routes. Catalog routes are `user` scope. Org administration enforces role from `X-User-Id` + path. `GET /api/organizations` and `GET /api/organizations/{organization_id}/members` name the resource in the path and do not use the caller.

Frontend session (cookie, current-org, subdomain) is your client. The request the gateway sees must still name the subject — org in the path for `organization` scope.

Trusting injected headers is a perimeter protocol: [`gateway-contract.md`](gateway-contract.md).

## Layers

| Layer | Question | Owner |
|-------|----------|--------|
| **Authentication** | Who is this? | Gateway + **IdP (JWT)** / identity API keys (credentials stripped before upstream) |
| **Organization context** | Is this credential an **active** member of this organization? | Gateway + **identity** (member resolve or member-scoped key) |
| **API key route scopes** | Does this restricted key share a label with `required_scopes`? | Gateway after admission. Restricted = non-null `scopes` (`[]` or labels). JWTs and `null` skip. Omitted `required_scopes` → any admitted principal. |
| **Resource authorization** | Can this member do X to project/doc/…? | **Business services** — not gateway headers |
| **Org administration** | Who may add or change members, manage service accounts, transfer ownership? | **identity** only (member role lives here) |

Route `required_scopes` is a credential intersection: the restricted key’s labels and the route’s labels must overlap.

## Route scopes → subject

| Scope | Subject after gateway | Identity headers |
|-------|----------------------|------------------|
| `public` | None | none |
| `user` | Person (`user_id`) | `X-User-Id` |
| `organization` | Member-in-org | `X-Organization-Id` + `X-Member-Id` only |

One subject per scope. Do not put `X-User-Id` on `organization` routes.

Always: `X-Request-ID`, `traceparent`. The edge may record `user.id` / `organization.id` / `member.id` on spans for ops — that is not the app contract. Org-scoped services do not receive `X-User-Id`.

## Member role (identity only)

Member **role** (member / admin / owner) is domain data for the **identity** service: add members, promote, owner rules, service accounts, member admin APIs. It is **not** gateway-injected identity and **not** part of the org-scope app contract.

Business services on `organization` scope get `organization_id` + `member_id` only. Role is not on the platform wire. Resource RBAC is the service’s problem.

## Who uses which scope

| Routes | Scope | Why |
|--------|-------|-----|
| User-centric APIs (user API keys, memberships, invite redeem) | **`user`** | Subject is the person. The person is not in the path. |
| **identity** catalog routes | **`user` only** | Membership **authority**. Must not sit behind gateway member resolve into itself. Org administration enforces role from `X-User-Id` + path. |
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
GET /api/user/memberships
Authorization: user JWT or user API key

Gateway: authn → inject X-User-Id → proxy
Service: subject is the person; list active memberships
```

Org administration is this shape (membership authority — must not sit behind member-resolve):

```
POST /api/organizations/{organization_id}/members
Authorization: user JWT or user API key

Gateway: authn → inject X-User-Id → proxy
identity: load member for (user_id, organization_id); enforce admin rules; respond
```

Directory reads name the resource in the path. The caller is not a subject.

```
GET /api/organizations
GET /api/organizations/{organization_id}/members
```

## Error split (locked)

| Case | HTTP / code |
|------|-------------|
| Bad or missing credential | **401** `UNAUTHORIZED` |
| Restricted API key missing route `required_scopes` | **403** `FORBIDDEN` |
| Non-member, unknown org, or member not `active` (org-context) | **404** `NOT_FOUND` |
| Admitted route or failed-auth IP over limit | **429** `RATE_LIMITED` |
| Member resolve / key validate down or timeout; Valkey down on a limited request; JWKS unavailable | **503** `SERVICE_UNAVAILABLE` |
| Missing expected identity headers on a protected route (downstream) | **500** `INTERNAL_ERROR` (platform bug) |

Existence policy: non-member and unknown org look the same (**404**) on identity org-administration routes. Directory reads do not use the caller. Unknown org on `GET /api/organizations/{organization_id}/members` is still **404**.

## Missing headers

| Scope | Expected headers | If missing |
|-------|------------------|------------|
| `user` | `X-User-Id` | `INTERNAL_ERROR` |
| `organization` | `X-Organization-Id`, `X-Member-Id` | `INTERNAL_ERROR` |
| `public` | none | — |

Do not return `UNAUTHORIZED` for missing identity headers — the gateway already authenticated (or should have rejected) the client.

## Invites

Org invites live in **identity** (`organization_invites`). Create/revoke are user-scope org-admin APIs. List is any active member; plaintext `token` only for admin/owner while the row is `active`. Redeem is user-scope `POST /api/invites/redeem` (authenticated invitee, `X-User-Id`). The host sends any email. Unknown token → 404. Redeemed / revoked / expired → 409 `CONFLICT` (`{ field: "status", value }`).

## What is not Plat5 identity (here)

- Project / document / generic resource ACL in the gateway
- FGA / ReBAC engines
- Tenant as a name for an organization
- Operator / employee admin planes
- Member role as platform wire identity (role stays in identity)
- Service accounts as a parallel auth system (they are members with keys)
- Multi-org service accounts
- SMTP in identity (invites return a token; the console sends mail if it wants)
- Pending member rows (invite redeem inserts an **active** member; add-by-`user_id` remains)
