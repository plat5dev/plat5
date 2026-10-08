# audit

Plat5 **audit** service: stores the org audit log the gateway writes, and serves it back to the org.

Contract: [`docs/audit.md`](../../docs/audit.md)

`routes.yml` is the gateway catalog for the read route. Apply it when audit is on.

## Layout

| Package | Role |
|---------|------|
| `events/` | Intent and outcome writes (internal), the org's list (public), monthly partitions |
| `db/migrations/` | `audit_events`, partitioned by month on `occurred_at`, with the trigger that keeps it append-only |

The gateway is the only writer. It sends the intent before forward (`PUT /internal/events/{request_id}`) and the outcome after (`PATCH`). Both are idempotent on `request_id`; an outcome applies once, `pending` → final. Audit never sees request bodies and does not interpret `details`.

Partitions run from last month to two months ahead, checked at boot and hourly. An intent outside them creates its month.

## Tests

Store tests need Postgres. They migrate a throwaway schema and drop it:

```bash
AUDIT_TEST_DATABASE_URL=postgres://plat5:plat5@localhost:5432/plat5?sslmode=disable go test ./...
```

Unset, they skip.

## Local

```bash
cd ../../compose && docker compose up --build audit
```
