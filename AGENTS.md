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
| Authn / org context / resource authz / org admin are separate layers | [`docs/identity-boundary.md`](docs/identity-boundary.md) |
| One subject per scope; org routes get org + member headers only | same + [`docs/gateway-contract.md`](docs/gateway-contract.md) |
| Identity public API is **`user` scope** — never behind its own org admission | [`docs/identity.md`](docs/identity.md) |
| Role stays in identity. Not a header. Not an internal “load role” path | identity-boundary |
| Service accounts are members with keys, not a parallel auth system | identity.md |
| A service account lives in exactly one org | identity.md |
| User keys and member keys are two products (prefix + table + validate URL) | identity.md |
| Add member by known `user_id` (immediate `active`) or invite redeem. No pending members | identity.md |
| Existence: unknown org / non-member / inactive → **404** | identity-boundary |
| Missing expected identity headers → **500** (gateway bug), not 401 | identity-boundary |
| JWT / IdP required to boot. API keys are an alternative credential, not an IdP-free mode | [`docs/idp-contract.md`](docs/idp-contract.md) |
| Identity **public** routes are operator-owned. Apply the catalog (or a subset). Not a seeded special case | [`docs/routes.md`](docs/routes.md), [`services/identity/routes.yml`](services/identity/routes.yml) |
| Internal validate/resolve stay on `INTERNAL_PORT`. Not optional via YAML | identity.md |
| Route-registry: Postgres desired state + revisions; etcd is the gateway projection | [`docs/route-registry.md`](docs/route-registry.md) |
| Apply is **upsert** of services in the file. Not prune | route-registry.md |
| etcd prefix: `edge/gateway/routes/` | routes.md |
| Gateway stop: no resource-type registry / relation graph / permission matrix | identity-boundary |

## Stop conditions

Do not add these because they would be convenient:

- `X-User-Id` or role on `organization` scope
- Gateway RBAC / FGA / project ACL
- Identity sitting on `organization` scope
- Platform-wide user directory or SMTP in identity
- Global / platform admin service accounts
- Multi-org service accounts (`home_organization_id`, SA member in a second org)
- Org `settings` / platform config bag
- Get role or user by `member_id` for org-scope apps
- Treating omitted identity routes as “feature off” (the process still serves them on the network)
- Auto-merge of new identity paths into existing operator YAML
- Shared `route-config` crate until a third consumer exists (two copies are deliberate)

## Deferred (not review findings)

| Item | Ready looks like |
|------|------------------|
| Admission cache invalidation / negative cache | Named invariant for revoke/suspend latency; misses don’t stampede identity |
| Apply `--prune` | Explicit CLI flag; not the default until that is the documented contract |
| Consumer libraries | Per-language helpers for headers + error envelope + missing-header → 500 |

Unfinished implementation of the above is not an architecture defect. A **doc that pretends they exist** is.

## Siblings

Wire or boot changes are not done in this repo alone. Update **cli** (embedded compose, generated `routes.identity.yml`, registry client), **template-***, **web-demo**, **toolbox** when the contract they generate, apply, or type changes.

CLI image pin (`plat5_version`) lags this tree until a plat5 release. Local `--plat5-compose` is how you run HEAD.

## Working here

1. Read the relevant `docs/*.md` before editing.
2. Hash design (invariants / stop / defer) before a non-trivial change.
3. Don’t special-case `identity` in the registry.
4. Don’t commit unless asked.
