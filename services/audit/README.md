# audit

Plat5 **audit** service: stores the audit log the gateway writes, one log per root plus each person's own, and serves it back by node or by user.

Contract: [`docs/concepts/audit.md`](../../docs/concepts/audit.md), [`docs/reference/api/audit.md`](../../docs/reference/api/audit.md). Storage: [`docs/internals/audit-storage.md`](../../docs/internals/audit-storage.md).

Its public routes come from audit's resources in `services/audit.yml` (`events` at every kind, `my_events` at `/user/audit-events`), expanded by the control plane ([resources](../../docs/reference/config/resources.md#audits-defaults)). Audit checks no permission: the gateway does.

## Layout

| Package | Role |
|---------|------|
| `events/` | Intent and outcome writes (internal), reads by node and the person's log (public), monthly partitions |
| `db/migrations/` | `audit_events` and `audit_log_keys`, partitioned by month on `occurred_at`, with the triggers that keep them append-only |

The gateway is the only writer. It sends the intent before forward (`PUT /internal/events/{request_id}`) and the outcome after (`PATCH`); its own refusals are one `PUT` that arrives final. Writes are idempotent on `request_id`; an outcome applies once, `pending` → final. Audit stores what it's sent: it doesn't repeat the gateway's rules, ignores unknown fields, and never interprets `details`. The internal port takes no token: it's reachable only inside the deployment's network.

Each event has one row in `audit_log_keys` per log it's in: each node of its chain, each node a user route's service named, the node it created, and on a user route `user:<user_id>`. Reads: `GET /nodes/{node_id}/audit-events` is a node's log, the node and everything below it; `GET /users/{user_id}/audit-events` is a person's own log. In a node's log a user route's event shows the named member as its actor and no user id.

Partitions run from last month to two months ahead, checked at boot and hourly. An intent outside them creates its month. Boot refuses a schema that records migrations this build doesn't know.

## Tests

Store tests need Postgres. They migrate a throwaway schema and drop it:

```bash
AUDIT_TEST_DATABASE_URL=postgres://plat5:plat5@localhost:5432/plat5?sslmode=disable go test ./...
```

Unset, they skip. `events/catalog_test.go` reads the control plane's gateway feed fixture (`services/control-plane/fixtures/catalog/gateway-feed.json`), serves every audit upstream in it with the ids the gateway fills, and checks what each read returns.

## Local

```bash
cd ../../compose && docker compose up --build audit
```
