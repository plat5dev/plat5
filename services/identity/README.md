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

A restricted caller cannot mint a wider key or member session. The gateway sends that caller's scopes in `X-Plat5-Scopes` (absent = unrestricted). Omitted scopes are copied from the caller. A label the caller does not have is **403** `INSUFFICIENT_SCOPE`. Contract: [`docs/identity.md`](../../docs/identity.md).

## Local

```bash
cd ../../compose && docker compose up --build identity
```
