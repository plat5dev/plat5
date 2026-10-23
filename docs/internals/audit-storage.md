# Audit storage

For implementers. The contract is [audit](../concepts/audit.md) and the [audit API](../reference/api/audit.md).

## Storage

`audit_events` in schema `audit` on the Plat5 Postgres. Partitioned by month on `occurred_at`. Unique on `request_id`; a GIN index on the node ids (`node_ids`, the chain beside `nodes`) for reads by node, and indexes on `named_root_id` and on the person's log (`actor_member_id` where `root_id` is null). Check constraints keep `root_id` the chain's head, a named root only on a sign-in event, and `key_prefix` set exactly for keys. Partitions are kept from last month to two months ahead, at boot and hourly.

One migration. Boot refuses a schema that records a migration this build doesn't know: drop it or use a fresh database.

Append-only, except the one `pending` → final transition. A trigger enforces it. No API updates or deletes an event. Events are kept: there's no retention yet, and purging a node doesn't delete its events.

## Runtime

| | |
|--|--|
| Directory | `services/audit/` |
| `service.name` / `service.namespace` | `audit` / `audit` |
| Public port | `3002` |
| Internal port | `3003` (`/health/*`, `/metrics`, `/internal/events`) |
| Database | Plat5 Postgres via `DATABASE_URL`, schema **`audit`** |
