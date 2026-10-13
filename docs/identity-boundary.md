# Identity Boundary

Authn, scope projection, and resource authz are separate layers. Mixing them breaks the platform model.

Every route declares which subject exists: none, the person, the org, or the member in that org. Those do not mix. The subject is who owns what the route addresses, never who called. Access to individual resources belongs to an optional policy engine, not to Plat5 and not to the service. The model and its reasons: [`model.md`](model.md).

Subject fill and service rules: [`gateway-contract.md`](gateway-contract.md). Routes: [`routes.md`](routes.md). Errors: [`api-errors.md`](api-errors.md). Identity APIs: [`identity.md`](identity.md).

## Why this cut

The route is a type. A handler does not get extra identity. Plat5 authenticates and admits. The scope drops fields. `upstream` is where the subject appears.

**One subject.** Billing, ACL, and admin checks that each key off a different id diverge. One subject per scope is the cut.

**What a member can reach.** `organization` has `organization_id`. `member` has `organization_id` and `member_id`. Neither has `user_id`: an org sees its members, not the directory. A check on one resource ("may this member touch doc D?") is the gateway's, through the policy-engine hook. A list of what a member can reach is a `member` route: the service gets `member_id` as its subject and asks the engine which resources it can reach ([`model.md`](model.md#what-i-can-reach)). The subject is what the answer is about, not who called.

**Roles are the deployment's, and they are not product ACL.** Plat5 names no roles. The deployment's roles file says which labels a role grants, and the gateway checks those labels against the route. Whether this member may touch this project is the optional policy engine's. Plat5 does not ship an owner / admin / member.

**User-subject routes stay on `user` scope.** `organization` and `member` do not have `user_id`. Identity's organization and member routes are published on those scopes because the template fills the subject from the credential. The handler reads the path. Who may call is the proxy.

Frontend session (cookie, current-org, subdomain) is your client. The client does not supply a subject id. The credential does.

Trusting the rewritten path is a perimeter protocol: [`gateway-contract.md`](gateway-contract.md).

## Layers

| Layer | Question | Owner |
|-------|----------|--------|
| **Authentication** | Who is this? | Gateway + **IdP (JWT)** / identity credentials: keys and sessions (stripped before upstream). Identity resolves a JWT's subject to the person |
| **Scope projection** | Which fields is this route allowed to see? | Gateway. The credential is the proof. The scope drops fields. |
| **Role resolution** | Which labels does this member's role grant? | Identity, from the deployment's roles file, at credential validate ([`roles.md`](roles.md)). The gateway never sees a role. |
| **Route labels** | Do the caller's labels share one with `required_labels`? | Gateway after admission, and only the gateway. A credential carries its principal's labels: a member key or session carries the member's role labels. A user has no role, so user credentials skip. Omitted `required_labels` → any admitted principal. |
| **Resource authorization** | Can this member do X to project/doc/…? Which projects can it reach? | The optional **policy engine**. The gateway asks it about one resource; a service asks it which resources its subject can reach. Neither Plat5 nor the service decides |
| **State rules** | Is this valid for the resource's current state? | **Business services**, over the subject in the path. Rules that hold whoever asks |
| **Who may call identity** | Who may add members, mint keys, delete an org? | Route labels on the identity catalog, at the gateway. Identity refuses illegal states. It does not compare the caller to what it grants. |

Route `required_labels` is an intersection: the caller's labels and the route's labels must overlap. One shared label is enough.

A credential is not narrower than its principal. Keys and sessions have no labels of their own. To give a machine less, give it a service account with a smaller role.

## Route scopes → subject

| Scope | Subject after gateway | Filled into `upstream` |
|-------|----------------------|------------------------|
| `public` | None | — |
| `user` | Person (`user_id`) | `{subject.user_id}` |
| `organization` | The org | `{subject.organization_id}` |
| `member` | The member in that org | `{subject.organization_id}`, `{subject.member_id}` |

One subject per scope. `organization` does not include `member_id` or `user_id`. `member` does not include `user_id`.

Always: `X-Request-ID`, `traceparent`. Services are not told the caller's labels or role. The route check already happened. The edge may record dropped ids on spans for ops — that is not the app contract.

## Roles

Plat5 ships no role names. A member carries a role slug, and the deployment's roles file says which labels it grants ([`roles.md`](roles.md)). Identity resolves the role at validate. The gateway checks labels and never sees a role. Services see neither. Roles are on or off for the whole deployment: with a roles file every member holds one of its roles; without one no member has a role and every member holds every label.

Assigning roles is a privilege like any other. Whoever may call a route that assigns a role, or creates a service account, may assign any role. The deployment decides who gets that label.

A role decides which routes a member may call. It does not decide what a member may do to a resource. Business services on `organization` scope get `organization_id` only. `member` scope gets `organization_id` and `member_id`. Access to individual resources is the policy engine's.

## Who uses which scope

| Routes | Scope | Why |
|--------|-------|-----|
| User-centric business APIs, and identity routes whose subject is the person | **`user`** | Subject is the person (`user_id`). |
| Org-scoped business APIs, and identity routes whose subject is the org | **`organization`** | Credential is a member of that org. Handler sees `organization_id`. |
| Routes whose subject is the member | **`member`** | Same credential. Handler sees `organization_id` and `member_id`. |

**Default:** trust gateway admission. Do not re-check “is this member in the org?” A service enforces what holds whoever asks: state rules, and scoping every lookup by the subject.

### Credentials

| Scope | Credential |
|-------|------------|
| `user` | User JWT or user API key |
| `organization`, `member` | Member API key or session |

Wrong credential for the scope is **401**. A user JWT does not become an org subject.

### Org-scoped API

```
GET /org/projects
X-API-Key: member key or member session

Gateway: admit → fill `{subject.organization_id}` into `upstream` → proxy
Service: the path is the subject; scope every lookup by it
```

If what the route addresses is the member's own (its keys, its comments, what it can reach), the route is `member` scope. Never to learn who called.

### User-scoped API

```
GET /user/widgets
Authorization: user JWT or user API key

Gateway: authn → fill `{subject.user_id}` into `upstream` → proxy
Service: subject is the person
```

### Identity

```
GET /users/{user_id}/memberships
POST /organizations/{organization_id}/members
PATCH /organizations/{organization_id}/members/{member_id}
PATCH /members/{member_id}
```

The path names every id the handler reads. The catalog's route labels decide who may call. Identity refuses illegal states: one membership row per user per org, last member, last `creator_role` holder, the invite machine, an address that does not exist, and a role not in the roles file.

## Error split (locked)

| Case | HTTP / code |
|------|-------------|
| Bad, missing, or wrong credential for the scope | **401** `UNAUTHORIZED` |
| Caller's labels miss route `required_labels` | **403** `FORBIDDEN` |
| Unknown id (identity handlers) | **404** `NOT_FOUND` |
| Admitted route or failed-auth IP over limit | **429** `RATE_LIMITED` |
| Credential validate down or timeout; Valkey down on a limited request; JWKS unavailable | **503** `SERVICE_UNAVAILABLE` |
| Path param is not one segment | **400** `INVALID_REQUEST` |
| Subject id is not one segment | **500** `INTERNAL_ERROR` |

Identity **404**s an unknown id, a removed member, and a key, member, or service account addressed under the wrong parent. It does not **404** "not a member." The gateway does not **404** a wrong credential. Inactive, expired, and unknown credentials are **401**.

## Invites

Org invites live in **identity** (`organization_invites`). Create, list, and revoke are `/organizations/{organization_id}/invites`. List includes `token` while `active`. Redeem is `POST /users/{user_id}/invites/redeem`. The host sends any email. Unknown token → 404. Redeemed / revoked / expired → 409 `CONFLICT` (`{ field: "status", value }`).

## What is not Plat5 identity (here)

- Project / document / generic resource ACL in the gateway
- FGA / ReBAC engines inside Plat5. One plugs in behind the policy-engine hook
- Tenant as a name for an organization
- Operator / employee admin planes
- Role names or meanings in Plat5. The deployment names roles in its roles file.
- Service accounts as a parallel auth system (they are members with keys)
- Service accounts tied to the member who created them. A service account is the org's. Its role is its own.
- Key or session labels narrower than the principal
- Telling services the caller's labels or role
- Multi-org service accounts
- SMTP in identity (invites return a token; the console sends mail if it wants)
- Pending member rows. Add by IdP subject and invite redeem both insert an **active** member.
