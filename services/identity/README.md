# identity

Plat5 **identity** service: organizations, members, invites, service accounts, user API keys, member API keys, and internal validate/resolve for the gateway.

Contract: [`docs/identity.md`](../../docs/identity.md)

Public route catalog: [`routes.yml`](routes.yml). Apply via route-registry; not auto-published in prod.

## Layout

| Package | Role |
|---------|------|
| `orgs/` | Organizations, members, invites, service accounts |
| `userkeys/` | User API keys (`{brand}-sk-1-`) |
| `memberkeys/` | Member API keys (`{brand}-mk-1-`) |

`APIKEY_BRAND` (default `plat5`) must match the gateway.

Identity does **not** send email and does not return a URL. List returns `token` for admin/owner while the invite is active. Auth does not carry `invite=`. No SMTP env vars.

## Local

```bash
cd ../../compose && docker compose up --build identity
```
