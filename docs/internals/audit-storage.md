# Audit storage

For implementers. The contract is [audit](../concepts/audit.md) and the [audit API](../reference/api/audit.md).

## Storage

`audit_events` in schema `audit` on the Plat5 Postgres, partitioned by month on `occurred_at`, unique on `request_id`. It holds the event as the [API](../reference/api/audit.md#event) returns it, with no check constraints between fields: audit stores what it's sent.

`audit_log_keys` is one row per log an event is in: `(log, event_id, occurred_at)`, where `log` is a node id (each node in `nodes`, each in `named_node_ids`, and `created_id`) or `user:` and a user id. A read is one index range on `(log, event_id DESC)`. The intent writes the chain's and the user's keys; the outcome adds the named nodes' and `created_id`'s. Partitioned with the events.

Partitions are kept from last month to two months ahead, at boot and hourly.

One migration. Boot refuses a schema that records a migration this build doesn't know: drop it or use a fresh database.

Append-only, except the one `pending` → final transition and the keys an outcome adds. A trigger enforces it. No API updates or deletes an event; retention is in the [concept](../concepts/audit.md#not-here).

## Runtime

| | |
|--|--|
| Directory | `services/audit/` |
| `service.name` / `service.namespace` | `audit` / `audit` |
| Public port | `3002` |
| Internal port | `3003` (`/health/*`, `/metrics`, `/internal/events`) |
| Database | Plat5 Postgres via `DATABASE_URL`, schema **`audit`** |
