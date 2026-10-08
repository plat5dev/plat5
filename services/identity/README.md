# identity

Plat5 **identity** service: organizations, members, invites, service accounts, user API keys, member API keys, member sessions, and internal validate for the gateway.

Contract: [`docs/identity.md`](../../docs/identity.md)

`routes.yml` is the gateway catalog. Apply it (or a subset). The process still serves unpublished paths on the public port.

## Layout

| Package | Role |
|---------|------|
| `orgs/` | Organizations, members, invites, service accounts |
| `userkeys/` | User API keys (`{brand}-sk-1-`) |
| `memberkeys/` | Member API keys (`{brand}-mk-1-`). The service-account path is the org address for the same rows. |
| `sessions/` | Member sessions (`{brand}-ms-1-`). Mint is in `routes.yml`. Validate stays internal. |

`APIKEY_BRAND` (default `plat5`) must match the gateway.

List includes `token` while the invite is active. The host sends any email.

A key or session carries its principal's permissions. It has no scopes of its own, and a mint that sends `scopes` is **422**. Identity does not know the caller's labels and does not compare the caller to what it grants: whoever the gateway admits to a route may assign any role. Contract: [`docs/identity.md`](../../docs/identity.md).

`ROLES_FILE` (optional) is the deployment's roles file, read at boot. Member key and session validate return the member's role labels. Unset → every member is unrestricted. Contract: [`docs/roles.md`](../../docs/roles.md).

## Local

```bash
cd ../../compose && docker compose up --build identity
```
