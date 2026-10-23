# Audit storage

For implementers. The contract is [audit](../concepts/audit.md) and the [audit API](../reference/api/audit.md).

## Storage

`audit_events` in schema `audit` on the Plat5 Postgres. Partitioned by month on `occurred_at`. Unique on `request_id`; a GIN index on the node ids for reads by node. Partitions are kept from last month to two months ahead, at boot and hourly.

Append-only, except the one `pending` → final transition. A trigger enforces it. No API updates or deletes an event. Events are kept: there's no retention yet, and purging a node doesn't delete its events.

## Runtime

| | |
|--|--|
| Directory | `services/audit/` |
| `service.name` / `service.namespace` | `audit` / `audit` |
| Public port | `3002` |
| Internal port | `3003` (`/health/*`, `/metrics`, `/internal/events`) |
| Database | Plat5 Postgres via `DATABASE_URL`, schema **`audit`** |
