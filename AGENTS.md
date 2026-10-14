# Plat5 — agent contract

`docs/` is law. If code and a contract disagree, fix the one that is wrong — usually the code. Do not invent a third story in chat.

How I decide: `my-principles` skill. This file is **what Plat5 is**. Do not copy philosophy here.

The model, and the reason behind each decision: [`docs/model.md`](docs/model.md). Read it before changing a lock below.

## What this is

A thin platform runtime: auth-delegating gateway, a private directory of people, org/member plane, audit, route publish.

Not an IdP, not FGA, not a hosted multi-tenant control plane. The directory holds ids, display names, and links to IdP subjects; customers cannot list it; staff can.

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
| Realms: the directory (every person, private) and organizations. Every credential belongs to one membership in one realm | [`docs/model.md`](docs/model.md), identity.md |
| One credential: user keys, member keys, and sessions share one table and one validate. Prefix is brand, kind (`k` key / `s` session), version; validate returns the realm. Version 1 prefixes still validate | identity.md |
| An organization validate result, and an org's view of a member, never carry `user_id` | identity.md |
| A person is created on first sign-in, from IdP issuer + subject. A person's `status` holds in every realm | identity.md |
| Add member by IdP subject (immediate `active`) or invite redeem. `user_id` stays internal | identity.md |
| A removed member is readable by id at the org address, as display name and status only. Account deletion is refused (**409** `ORGANIZATION_HANDOFF_REQUIRED`) while the person is an org's last member or `creator_role` holder | identity.md |
| `removed` is final: a remove revokes the member's keys and deletes its sessions. Adding the person again is a new member id. `suspended` is the reversible state | identity.md |
| Unknown id is **404**. Wrong credential for the scope is **401**, not 404 | identity-boundary |
| Subject id is not one segment → **500**. Path param is not one segment → **400** | identity-boundary |
| JWT / IdP required to boot. API keys are an alternative credential | [`docs/idp-contract.md`](docs/idp-contract.md) |
| Identity **public** routes are operator-owned. Apply the catalog (or a subset) | [`docs/routes.md`](docs/routes.md), [`services/identity/routes.yml`](services/identity/routes.yml) |
| Internal validate stays on `INTERNAL_PORT`. Gateway boots with `CREDENTIAL_VALIDATE_URL`. `APIKEY_BRAND` is identity's alone; the gateway checks a key's shape, not its brand | identity.md, gateway-contract.md |
| Lifecycle events: identity writes births and ends of organizations and members (`organization.created`, `organization.deleted`, `member.created`, `member.removed`) in the same transaction as the change. Org create also writes `member.created` for the founder, after `organization.created`. Account deletion writes `member.removed` per org and nothing for the directory. Always recorded; services opt in by reading the internal feed. Event writes are serialized, so `id` order is commit order | identity.md |
| Route-registry: Postgres desired state + revisions; etcd is the gateway projection | [`docs/route-registry.md`](docs/route-registry.md) |
| Apply is **upsert** of services in the file. Services not in the file are left alone | route-registry.md |
| etcd prefix: `edge/gateway/routes/` | routes.md |
| Named rate-limit policies on the service; `shared: true` is opt-in cross-service; limiter subject follows route scope | [`docs/routes.md`](docs/routes.md), [`docs/gateway-contract.md`](docs/gateway-contract.md) |
| Rate-limit counters in Valkey; replicas share one budget; Valkey required to boot; fail-closed 503 | [`docs/gateway-contract.md`](docs/gateway-contract.md), [`docs/routes.md`](docs/routes.md) |
| Admission cache in-process (positive + negative); singleflight; TTL is revoke/suspend/role-change latency | [`docs/gateway-contract.md`](docs/gateway-contract.md), [`docs/identity.md`](docs/identity.md) |
| Gateway admits and fills the route subject into `upstream`. Access to individual resources is the optional policy engine's: the gateway asks it about one resource; a service asks it, on a `member` route, which resources its subject can reach. Until the hook ships, see [`TODO.md`](TODO.md) | identity-boundary, model.md |
| Services see owners, never actors. No actor header and no creator concept: who-did-it that matters is a member-owned record; the rest is audit | model.md |
| Roles are the deployment's: `ROLES_FILE` on identity, read at boot. On or off for the whole deployment: with a file every member holds one of its roles; without one no member has a role and every member holds every label. Plat5 names no roles. Identity resolves the role at validate. The gateway sees labels only | [`docs/roles.md`](docs/roles.md) |
| A credential carries its membership's permissions. Member keys and sessions carry the member's role labels. User credentials have no labels to check. Keys and sessions have no labels of their own | roles.md |
| No grant cap. Whoever may call a route may assign any role and act on any member. The route's labels are the only check | roles.md |
| The gateway is the only access check before a service. Services are not told the caller's role or labels | [`docs/gateway-contract.md`](docs/gateway-contract.md) |
| A member has two addresses: `/members/{member_id}` (itself) and `/organizations/{organization_id}/members/{member_id}` (the org acting on it) | identity.md |
| Gateway order: authenticate, rate limit, audit intent, `required_labels`, substitute. A label-denied 403 counts against the rate limit | [`docs/gateway-contract.md`](docs/gateway-contract.md), [`docs/audit.md`](docs/audit.md) |
| An audit event is one request, in the log of the realm that owns what it changed: the route subject's org, or on a `user` route the person's own (and the org the service names on success). Not `public` routes. Writes by default; route `audit` overrides | audit.md, routes.md |
| Audit intent before forward, awaited. Not written → 503, service not called. Outcome after, in the background, idempotent on `request_id`, `pending` → final once. Pending is unknown, never success | audit.md |
| Services say what changed on `X-Plat5-Audit-Details`, the id a create produced on `X-Plat5-Audit-Created`, and on a `user` route the org and member it changed on `X-Plat5-Audit-Organization` / `-Member`. The gateway stores them and always strips them. Identity reports changes as `from`/`to` and creates by id, never a secret or `user_id` | audit.md, identity.md |
| Product features never read audit. Audit is reporting and compliance | audit.md, model.md |
| Audit is on or off for the whole deployment. `AUDIT_URL` required unless `AUDIT_ENABLED=false` | audit.md |

## Stop conditions

Do not add these because they would be convenient:

- `user_id` on `organization` or `member` scope. `member_id` on `organization` scope
- Role resolution, FGA, or project ACL in the gateway. The gateway checks labels, and asks the optional engine through the hook
- Nodes, bindings, or a hierarchy inside an organization. Projects are resources, not Plat5 concepts
- A policy engine (Topaz, OPA, …) in the default stack
- User-subject identity routes on `organization` or `member` scope
- A customer route that lists or searches people, or SMTP in identity
- Global / platform admin service accounts
- Multi-org service accounts (`home_organization_id`, SA member in a second org)
- Org `settings` / platform config bag
- An organization slug, or any second human key. The id is the address; `name` is the label
- Role names or meanings in Plat5 code (`owner` / `admin` / `member`), or getting a user id from `member_id` for org-scope apps
- Roles in the route registry, etcd, or the gateway
- More than one role per member, or per-org custom roles (deferred)
- Telling services the caller's role or labels
- Key or session labels, or any other way to narrow a credential below its principal
- A member with no role while roles are on, or a mix of roles on and off
- The word `scopes` for labels on the wire. Route `scope` is `public` / `user` / `organization` / `member`; permissions are `labels`
- A grant cap, or any identity check that compares the caller to the role it assigns or the member it acts on
- Reviving a `removed` member, or keys or sessions that outlive a remove
- `user_id` in an organization validate result, in an org's view of a member, or as the way to add one
- A credential table, prefix, or validate URL per kind of principal, or the realm in a credential's prefix
- The brand in the gateway or in clients
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
- Request or response bodies, query strings, labels, or role in an audit event, or `user_id` in an org's view of one
- Audit events from `public` routes, or an org log chosen by a path param
- 401s or 429s in the audit log
- The gateway reading inside audit details, or a service header creating or re-outcoming an event, or changing who acted
- An actor header, or any other way a service learns who called
- A creator field (`created_by`) on an org-owned resource. Who-did-it that matters is a member-owned record ([`docs/model.md`](docs/model.md#who-did-it))
- A product feature that reads the audit log
- Changing an audit event after its outcome, deleting events through an API, or turning `pending` into anything else after the fact
- Audit on for some orgs or services and off for others
- Audit events in Valkey or etcd
- A queue, broker, or push delivery in identity for lifecycle events. Push is a relay that reads the feed
- Lifecycle events that are not written in the same transaction as the change, or that carry `user_id`
- Lifecycle events other than births and ends of organizations and members, or a name or role on one. Updates, suspends, the directory, credentials, and invites are not events
- Deriving lifecycle events from the audit log

## Siblings

Wire or boot changes are not done in this repo alone. Update **cli** (embedded compose, generated `routes.identity.yml`, registry client), **template-***, **web-demo**, **toolbox** when the contract they generate, apply, or type changes.

CLI image pin (`plat5_version`) lags this tree until a plat5 release. Local `--plat5-compose` is how you run HEAD.

## Working here

1. Read the relevant `docs/*.md` before editing.
2. Hash design (invariants / stop / defer) before a non-trivial change.
3. Don’t special-case `identity` in the registry.
4. Don’t commit unless asked.
