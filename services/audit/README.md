# audit

Plat5 **audit** service: stores the audit log the gateway writes, one log per root plus each person's own, and serves it back by node.

Contract: [`docs/concepts/audit.md`](../../docs/concepts/audit.md), [`docs/reference/api/audit.md`](../../docs/reference/api/audit.md). Storage: [`docs/internals/audit-storage.md`](../../docs/internals/audit-storage.md).

Its edge routes are generated from the kinds into the deployment's `routes/audit.yml` (`plat5 routes generate`); apply them when audit is on.

## Layout

| Package | Role |
|---------|------|
| `events/` | Intent and outcome writes (internal), reads by node and the person's log (public), monthly partitions |
| `db/migrations/` | `audit_events`, partitioned by month on `occurred_at`, with the trigger that keeps it append-only |

The gateway is the only writer. It sends the intent before forward (`PUT /internal/events/{request_id}`) and the outcome after (`PATCH`). Both are idempotent on `request_id`; an outcome applies once, `pending` → final. Audit never sees request bodies and does not interpret `details`.

Reads: `GET /nodes/{node_id}/audit-events` is the node and everything below it, plus the sign-in events that named it as their root; `GET /members/{member_id}/audit-events` is a person's own log, keyed by their directory member.

Partitions run from last month to two months ahead, checked at boot and hourly. An intent outside them creates its month. Boot refuses a v1 audit schema.

## Tests

Store tests need Postgres. They migrate a throwaway schema and drop it:

```bash
AUDIT_TEST_DATABASE_URL=postgres://plat5:plat5@localhost:5432/plat5?sslmode=disable go test ./...
```

Unset, they skip. `events/catalog_test.go` reads the control plane's catalog fixture (`services/control-plane/fixtures/catalog/identity-feed.json`) and serves every audit upstream in it; identity's catalog test checks those routes' labels.

## Local

```bash
cd ../../compose && docker compose up --build audit
```
