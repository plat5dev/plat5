# Audit storage

For implementers. The contract is [audit](../concepts/audit.md), the [audit API](../reference/api/audit.md), and the [internal export API](../reference/api/internal.md#export).

## Storage

`audit_events` in schema `audit` on the Plat5 Postgres, partitioned by month on `occurred_at`, unique on `(request_id, occurred_at)`. Intent dedupe bounds its `request_id` check to `±1 day` around `occurred_at` so Postgres prunes to at most two partitions; outcome updates include `occurred_at` when sent so Postgres prunes to the single partition. It holds the event as the [API](../reference/api/audit.md#event) returns it, with no check constraints between fields: audit stores what it's sent.

`finalized_seq` is assigned from sequence `export_seq` under a transaction-scoped advisory lock (`audit.export_seq`) only when an event becomes final (on `ApplyOutcome` or a single-write final `CreateIntent`), guaranteeing strict commit-order visibility for deployment-wide `GET /internal/events` export readers.

`audit_log_keys` is one row per log an event is in: `(log, event_id, occurred_at)`, where `log` is a node id (each node in `nodes`, each in `named_node_ids`, and `created_id`) or `user:` and a user id. A read is one index range on `(log, event_id DESC)`. The intent writes the chain's and the user's keys; the outcome adds the named nodes' and `created_id`'s. Partitioned with the events.

Partitions are kept from last month to two months ahead, checked at boot and hourly.

Migrations refuse a schema that records a migration this build doesn't know: drop it or use a fresh database.

Append-only, except the one `pending` → final transition (which also sets `finalized_seq`) and the keys an outcome adds. Triggers enforce it. No API updates or deletes an event. Dropping expired whole monthly partitions via `audit prune` runs under the schema `Owner` role and does not fire row delete triggers.

## Roles

| Role | Can | Used by |
|------|-----|---------|
| Owner | Owns schema `audit`. Runs migrations, owns `ensure_partitions`, and drops expired monthly partitions | `audit migrate`, `audit prune` |
| Writer | `SELECT`, `INSERT`, column-limited `UPDATE` on `audit_events`, `SELECT`/`INSERT` on `audit_log_keys`, `USAGE`/`SELECT` on `export_seq`, and `EXECUTE` on `ensure_partitions(timestamptz)`. Nothing else: no `DELETE`, `TRUNCATE`, DDL, or ownership | `audit serve` |

The deployment creates both login roles. `migrate` grants the role named by `WRITER_ROLE` exactly the writer's rights. `serve` refuses to boot if its role owns schema `audit`, or can delete from or truncate `audit_events` or `audit_log_keys`.

`ensure_partitions(timestamptz)` is `SECURITY DEFINER`, owned by the owner role, so the writer can add a month without DDL rights.

The trigger stops the writer. It doesn't stop the owner or a superuser. Neither credential is used at runtime.

## Pruning

`audit prune` runs as the schema owner and drops whole monthly partitions of `audit_log_keys` and `audit_events` strictly older than the cutoff:

```bash
audit prune --before 2026-01 --dry-run
audit prune --older-than 12m
```

Specify exactly one of `--before YYYY-MM` or `--older-than <N>m`. Cutoffs past the current UTC month are refused so live or future partitions cannot be dropped.

## Runtime

| | |
|--|--|
| Directory | `services/audit/` |
| Commands | `audit migrate`, `audit serve`, `audit prune` |
| `service.name` / `service.namespace` | `audit` / `audit` |
| Public port | `3002` |
| Internal port | `3003` (`/health/*`, `/metrics`, `/internal/events`) |
| Database | Plat5 Postgres via `DATABASE_URL`, schema **`audit`** |
