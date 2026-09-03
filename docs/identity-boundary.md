# Identity Boundary

Authn, organization context, and resource authz are separate layers. Mixing them breaks the platform model.

Headers and service rules: [`gateway-contract.md`](gateway-contract.md). Routes: [`routes.md`](routes.md). Errors: [`api-errors.md`](api-errors.md). Identity APIs: [`identity.md`](identity.md).

## Layers

| Layer | Question | Owner |
|-------|----------|--------|
| **Authentication** | Who is this? | Gateway + **IdP (JWT)** / identity API keys (credentials stripped before upstream) |
| **Organization context** | Is this credential an **active** member of this organization? | Gateway + **identity** (member resolve or member-scoped key) |
| **API key route scopes** | Does this restricted key share a label with `required_scopes`? | Gateway after admission. Restricted = non-null `scopes` (`[]` or labels). JWTs and `null` skip. Omitted `required_scopes` → any admitted principal. Not resource ACL. |
| **Resource authorization** | Can this member do X to project/doc/…? | **Business services** — not gateway headers |
| **Org administration** | Who may add or change members, manage service accounts, transfer ownership? | **identity** only (member role lives here) |

**Stop condition for the gateway:** If a check needs a resource-type registry, relation graph, or permission matrix on the wire, it does not belong in the gateway. Route `required_scopes` is a credential intersection, not that.

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
| **identity** public APIs | **`user` only** | Membership **authority**. Must not sit behind gateway member resolve into itself. Enforces member and admin rules in-process from `X-User-Id` + path. |
| Business APIs under an org path | **`organization`** | Gateway admits active member; service trusts org headers and enforces resource authz. |
| User-centric APIs (user API keys, list my orgs) | **`user`** | Subject is the person. |

**Default on `organization` scope:** trust gateway admission — do not re-check “is this member in the org?” Enforce **resource** authz in the service. Re-checking admission is optional defense-in-depth, not required.

### Credentials on `organization` scope

| Credential | Admission |
|------------|-----------|
| User JWT | Authn → `user_id` → **member resolve** `(user_id, organization_id)` → inject org headers |
| User API key | Validate → `user_id` (+ `scopes`) → same resolve → inject → `required_scopes` if the key is restricted |
| Member API key | Validate → `member_id` + `organization_id` + `scopes` → path org must match + member active → inject (no resolve call) → `required_scopes` if restricted |

### Default multi-org UX (business route)

```
GET /api/organizations/{organization_id}/projects
Authorization: user JWT or user API key
  (or X-API-Key: member-scoped key for automation)

Gateway: authn → admit active member → inject X-Organization-Id + X-Member-Id → proxy
Service: trust those headers; enforce resource authz as needed
```

No organization-scoped token exchange required for the default path.

### Identity service (authority)

```
GET /api/organizations/{organization_id}/members
Authorization: user JWT or user API key

Gateway: authn → inject X-User-Id → proxy
identity: load member for (user_id, organization_id); enforce admin rules; respond
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

Existence policy: non-member and unknown org look the same (**404**), including on identity service org resources.

## Missing headers

| Scope | Expected headers | If missing |
|-------|------------------|------------|
| `user` | `X-User-Id` | `INTERNAL_ERROR` |
| `organization` | `X-Organization-Id`, `X-Member-Id` | `INTERNAL_ERROR` |
| `public` | none | — |

Do not return `UNAUTHORIZED` for missing identity headers — the gateway already authenticated (or should have rejected) the client.

## Invites

Org invites live in **identity** (`organization_invites`). Create/revoke are user-scope org-admin APIs. List is any active member; plaintext `token` only for admin/owner while the row is `active`. Redeem is user-scope `POST /api/invites/redeem` (authenticated invitee, `X-User-Id`). Identity does not return a URL and does not send email. Auth does not carry `invite=`. Unknown token → 404. Redeemed / revoked / expired → 409 `CONFLICT` (`{ field: "status", value }`).

## What is not Plat5 identity (here)

- Project / document / generic resource ACL in the gateway
- FGA / ReBAC engines
- **Tenant** (hosted Plat5 customer account) — different word, different product; not Plat5
- Operator / employee admin planes
- Member role as platform wire identity (role stays in identity)
- Service accounts as a parallel auth system (they are members with keys)
- Multi-org service accounts
- SMTP in identity (invites return a token; the console sends mail if it wants)
- Pending member rows (invite redeem inserts an **active** member; add-by-`user_id` remains)
