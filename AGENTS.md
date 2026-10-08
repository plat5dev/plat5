# Plat5 — agent contract

`docs/` is law. If code and a contract disagree, fix the one that is wrong — usually the code. Do not invent a third story in chat.

How I decide: `my-principles` skill. This file is **what Plat5 is**. Do not copy philosophy here.

## What this is

A thin platform runtime: auth-delegating gateway, org/member plane, route publish.

Not a user directory, not an IdP, not FGA, not a hosted multi-tenant control plane.

## Locked

Read the doc, don’t re-derive:

| Invariant | Where |
|-----------|--------|
| Authn / scope projection / resource authz are separate layers. Who may call identity is the proxy | [`docs/identity-boundary.md`](docs/identity-boundary.md) |
| One subject per scope. `user` fills `user_id`. `organization` fills `organization_id`. `member` fills `organization_id` and `member_id`. Delivered by `{subject.*}` in `upstream` | same + [`docs/gateway-contract.md`](docs/gateway-contract.md) |
| Identity is a function of the URL. Who may call is the proxy. User-subject routes stay on `user` scope | [`docs/identity.md`](docs/identity.md) |
| Service accounts are members with keys | identity.md |
| A service account lives in exactly one org | identity.md |
| A service account is the org's. Its role is its own, not bounded by whoever created it | identity.md, roles.md |
| User keys and member keys are two products (prefix + table + validate URL) | identity.md |
| Member sessions are not member keys (own prefix, table, validate URL). Validate does not return `user_id` | identity.md |
| Add member by known `user_id` (immediate `active`) or invite redeem | identity.md |
| Unknown id is **404**. Wrong credential for the scope is **401**, not 404 | identity-boundary |
| Subject id is not one segment → **500**. Path param is not one segment → **400** | identity-boundary |
| JWT / IdP required to boot. API keys are an alternative credential | [`docs/idp-contract.md`](docs/idp-contract.md) |
| Identity **public** routes are operator-owned. Apply the catalog (or a subset) | [`docs/routes.md`](docs/routes.md), [`services/identity/routes.yml`](services/identity/routes.yml) |
| Internal validate stays on `INTERNAL_PORT`. Gateway boots with the three validate URLs | identity.md |
| Route-registry: Postgres desired state + revisions; etcd is the gateway projection | [`docs/route-registry.md`](docs/route-registry.md) |
| Apply is **upsert** of services in the file. Services not in the file are left alone | route-registry.md |
| etcd prefix: `edge/gateway/routes/` | routes.md |
| Named rate-limit policies on the service; `shared: true` is opt-in cross-service; limiter subject follows route scope | [`docs/routes.md`](docs/routes.md), [`docs/gateway-contract.md`](docs/gateway-contract.md) |
| Rate-limit counters in Valkey; replicas share one budget; Valkey required to boot; fail-closed 503 | [`docs/gateway-contract.md`](docs/gateway-contract.md), [`docs/routes.md`](docs/routes.md) |
| Admission cache in-process (positive + negative); singleflight; TTL is revoke/suspend/role-change latency | [`docs/gateway-contract.md`](docs/gateway-contract.md), [`docs/identity.md`](docs/identity.md) |
| Gateway admits and fills the route subject into `upstream`. Resource authz stays in the service, or a policy engine it calls | identity-boundary |
| Roles are the deployment's: `ROLES_FILE` on identity, read at boot. On or off for the whole deployment: with a file every member holds one of its roles; without one no member has a role and every member holds every label. Plat5 names no roles. Identity resolves the role at validate. The gateway sees labels only | [`docs/roles.md`](docs/roles.md) |
| A credential carries its principal's permissions. Member keys and sessions carry the member's role labels. User credentials have no labels to check. Keys and sessions have no labels of their own | roles.md |
| No grant cap. Whoever may call a route may assign any role and act on any member. The route's labels are the only check | roles.md |
| The gateway is the only access check before a service. Services are not told the caller's role or labels | [`docs/gateway-contract.md`](docs/gateway-contract.md) |
| A member has two addresses: `/members/{member_id}` (itself) and `/organizations/{organization_id}/members/{member_id}` (the org acting on it) | identity.md |

## Stop conditions

Do not add these because they would be convenient:

- `user_id` on `organization` or `member` scope. `member_id` on `organization` scope
- Role resolution, FGA, or project ACL in the gateway. The gateway checks labels only
- A policy engine (Topaz, OPA, …) in the default stack
- User-subject identity routes on `organization` or `member` scope
- Platform-wide user directory or SMTP in identity
- Global / platform admin service accounts
- Multi-org service accounts (`home_organization_id`, SA member in a second org)
- Org `settings` / platform config bag
- Role names or meanings in Plat5 code (`owner` / `admin` / `member`), or getting a user id from `member_id` for org-scope apps
- Roles in the route registry, etcd, or the gateway
- More than one role per member, or per-org custom roles (deferred)
- Telling services the caller's role or labels
- Key or session labels, or any other way to narrow a credential below its principal
- A member with no role while roles are on, or a mix of roles on and off
- The word `scopes` for labels on the wire. Route `scope` is `public` / `user` / `organization` / `member`; permissions are `labels`
- A grant cap, or any identity check that compares the caller to the role it assigns or the member it acts on
- Folding member sessions into `member_api_keys`, or returning `user_id` from session validate
- A service-account key table, prefix, or validate URL (those keys are member keys)
- Treating omitted identity routes as “feature off” (the process still serves them on the network)
- Auto-merge of new identity paths into existing operator YAML
- Shared `route-config` crate until a third consumer exists (two copies are deliberate)
- Rate-limit policy catalog (separate apply / etcd resource)
- Limiter subject override (ip / user / org / member comes from route scope)
- Billing, usage, or meter fields on `rate_limit` / `rate_limits`
- Cost weights, multi-window, burst, or calendar quotas on the gateway limiter
- JWT / JWKS / admission caches in Valkey (Valkey is the rate-limit store)
- In-process rate-limit fallback when Valkey is down
- `Strict-Transport-Security` on the gateway (TLS is the edge)

## Siblings

Wire or boot changes are not done in this repo alone. Update **cli** (embedded compose, generated `routes.identity.yml`, registry client), **template-***, **web-demo**, **toolbox** when the contract they generate, apply, or type changes.

CLI image pin (`plat5_version`) lags this tree until a plat5 release. Local `--plat5-compose` is how you run HEAD.

## Working here

1. Read the relevant `docs/*.md` before editing.
2. Hash design (invariants / stop / defer) before a non-trivial change.
3. Don’t special-case `identity` in the registry.
4. Don’t commit unless asked.
