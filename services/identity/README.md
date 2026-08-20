# identity

Plat5 **identity** service: organizations, members, service accounts, user API keys, member API keys, and internal validate/resolve for the gateway.

Contract: [`docs/identity.md`](../../docs/identity.md)

Public route catalog: [`routes.yml`](routes.yml). Apply via route-registry; not auto-published in prod.

## Layout

| Package | Role |
|---------|------|
| `orgs/` | Organizations, members, service accounts |
| `userkeys/` | User API keys (`{brand}-sk-1-`) |
| `memberkeys/` | Member API keys (`{brand}-mk-1-`) |

`APIKEY_BRAND` (default `plat5`) must match the gateway.

## Local

```bash
cd ../../compose && docker compose up --build identity
```
