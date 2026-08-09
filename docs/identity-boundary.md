# Identity Boundary

Authn, organization context, and resource authz are separate layers. Mixing them breaks the platform model.

Headers and service rules: [`gateway-contract.md`](gateway-contract.md). Routes: [`routes.md`](routes.md). Errors: [`api-errors.md`](api-errors.md).

## Layers

| Layer | Question | Owner |
|-------|----------|--------|
| **Authentication** | Who is this? | Gateway + **IdP (JWT)** / api-keys (credentials stripped before upstream) |
| **Organization context** | Is this user an **active** member of this organization? | Gateway + **organizations** (membership resolve) |
| **Resource authorization** | Can this membership do X to project/doc/…? | **Business services** — not gateway headers |
| **Org administration** | Who may invite, change membership, transfer ownership? | **organizations** only (membership role lives here) |

**Stop condition for the gateway:** If a check needs a resource-type registry, relation graph, or permission matrix on the wire, it does not belong in the gateway.

## Route scopes → subject

| Scope | Subject after gateway | Identity headers |
|-------|----------------------|------------------|
| `public` | None | none |
| `user` | Person (`user_id`) | `X-User-Id` |
| `organization` | Membership-in-org | `X-Organization-Id` + `X-Membership-Id` only |

One subject per scope. Do not put `X-User-Id` on `organization` routes.

Always: `X-Request-ID`, `traceparent`. The edge may record `user.id` / `organization.id` / `membership.id` on spans for ops — that is not the app contract. Org-scoped services do not receive `X-User-Id`.

## Membership role (organizations only)

Membership **role** (member / admin / owner) is domain data for the **organizations** service: invite, promote, owner rules, membership admin APIs. It is **not** gateway-injected identity and **not** part of the org-scope app contract.

Business services on `organization` scope get `organization_id` + `membership_id` only. Need role later → load it from organizations, not a header.

## Who uses which scope

| Routes | Scope | Why |
|--------|-------|-----|
| **organizations** public APIs | **`user` only** | Membership **authority**. Must not sit behind gateway membership resolve into itself. Enforces membership and admin rules in-process from `X-User-Id` + path. |
| Business APIs under an org path | **`organization`** | Gateway admits active membership; service trusts org headers and enforces resource authz. |
| User-centric APIs (keys, profile, list my orgs) | **`user`** | Subject is the person. |

**Default on `organization` scope:** trust gateway admission — do not re-check “is this membership in the org?” Enforce **resource** authz in the service. Re-checking admission is optional defense-in-depth, not required.

### Default multi-org UX (business route)

```
GET /api/organizations/{organization_id}/projects
Authorization: user JWT or user API key

Gateway: authn → membership resolve → inject X-Organization-Id + X-Membership-Id → proxy
Service: trust those headers; enforce resource authz as needed
```

No organization-scoped token exchange required for the default path.

### Organizations service (authority)

```
GET /api/organizations/{organization_id}/memberships
Authorization: user JWT or user API key

Gateway: authn → inject X-User-Id → proxy
organizations: load membership for (user_id, organization_id); enforce admin rules; respond
```

## Error split (locked)

| Case | HTTP / code |
|------|-------------|
| Bad or missing credential | **401** `UNAUTHORIZED` |
| Non-member, unknown org, or membership not `active` (org-context) | **404** `NOT_FOUND` |
| Membership resolve down / timeout | **503** `SERVICE_UNAVAILABLE` |
| Missing expected identity headers on a protected route (downstream) | **500** `INTERNAL_ERROR` (platform bug) |

Existence policy: non-member and unknown org look the same (**404**), including on organizations service org-scoped resources.

## Missing headers

| Scope | Expected headers | If missing |
|-------|------------------|------------|
| `user` | `X-User-Id` | `INTERNAL_ERROR` |
| `organization` | `X-Organization-Id`, `X-Membership-Id` | `INTERNAL_ERROR` |
| `public` | none | — |

Do not return `UNAUTHORIZED` for missing identity headers — the gateway already authenticated (or should have rejected) the client.

## What is not Plat5 identity (here)

- Project / document / generic resource ACL in the gateway
- FGA / ReBAC engines
- **Tenant** (hosted Plat5 customer account) — different word, different product; not Plat5
- Operator / employee admin planes
- Membership role as platform wire identity (role stays in organizations)
