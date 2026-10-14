# identity

Plat5 **identity** service: the directory of people, organizations, members, invites, service accounts, credentials (API keys and sessions), internal credential validate for the gateway, and the internal lifecycle-events feed for services.

Contract: [`docs/identity.md`](../../docs/identity.md)

`routes.yml` is the gateway catalog. Apply it (or a subset). The process still serves unpublished paths on the public port.

## Layout

| Package | Role |
|---------|------|
| `directory/` | People: IdP subject resolve (first sign-in creates the person), `GET` / `PATCH /users/{user_id}` |
| `orgs/` | Organizations, members, invites, service accounts, account deletion |
| `credentials/` | Every API key and session (`{brand}-k-2-`, `{brand}-s-2-`): user keys, member keys, the service-account address for the same rows, session mint, and the one internal validate |
| `lifecycle/` | Lifecycle events (`organization.created`, `organization.deleted`, `member.created`, `member.removed`). Written in the change's transaction; read on the internal port. |

`APIKEY_BRAND` (default `plat5`) is identity's alone. The gateway checks a key's shape, not its brand.

List includes `token` while the invite is active. The host sends any email.

A key or session carries its principal's permissions. It has no labels of its own. Identity does not know the caller's labels and does not compare the caller to what it grants: whoever the gateway admits to a route may assign any role. Contract: [`docs/identity.md`](../../docs/identity.md).

Roles are on unless `ROLES_ENABLED=false`. On, identity reads the roles document from the control plane (`CONTROL_PLANE_URL`), serves nothing until it has one, and picks up each change without a restart. Every member holds one of its roles, and validate returns that role's labels for a member's key or session. Off: no member has a role, and validate returns `["*"]` (every label). Contract: [`docs/roles.md`](../../docs/roles.md).

## Local

```bash
cd ../../compose && docker compose up --build identity
```
