# organizations → identity

Implementation tree for the Plat5 **identity** service (rename to `services/identity/` pending).

Contract: [`docs/identity.md`](../../docs/identity.md)

## Phase 1 (done)

- Schema `identity`: `organizations`, `members` (user XOR service_account), `service_accounts` table present
- Public API: `/api/organizations`, `/api/organizations/{id}/members`
- Internal: `POST /internal/members/resolve` → `{ member_id, organization_id, user_id, status }`
- Member JSON: `principal`, `user_id`, `service_account_id`

Not yet: user/member API keys, service-account CRUD routes.

## Local

```bash
cd ../../compose && docker compose up --build organizations

export DATABASE_URL=postgres://plat5:plat5@localhost:5432/plat5?sslmode=disable
go run .
```

## Env

| Var | Default | Purpose |
|-----|---------|---------|
| `PORT` | `3000` | Public API |
| `INTERNAL_PORT` | `3001` | Health, `/metrics`, resolve |
| `INTERNAL_AUTH_TOKEN` | unset | When set, require `X-Plat5-Internal-Token` |
| `DATABASE_URL` | local postgres | Schema `identity` |
| `OTEL_SERVICE_NAME` | `identity` | Resource `service.name` |
