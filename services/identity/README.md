# identity

Plat5 **identity**: the tree of nodes, and the principals, members, bindings, keys and invites in it.

Contracts: [identity API](../../docs/reference/api/identity.md), [lifecycle](../../docs/concepts/lifecycle.md), [schema](../../docs/internals/identity-schema.md). Its gateway routes are generated per kind into the deployment's `routes/identity.yml` (`plat5 routes generate`).

## Layout

| Package | Role |
|---------|------|
| `feed/` | The config from the control plane's `GET /internal/identity`: kinds, roles, `deletion_grace`, `reserved_slugs`, and the labels of the `identity` service's node routes, which filter node lists. Identity doesn't listen until it has one, and long-polls for changes |
| `nodes/` | Nodes, the closure table, slugs and tombstones, soft delete, restore, purge, kind removal, and the sweeper that runs reconcile and purge |
| `members/` | People and IdP links, directory members, members of each tree, bindings with expiry, the access list, service accounts, API keys, invites and redeem; creating nodes with the creator's binding, and filtered node lists. Its sweep deletes expired bindings |
| `lifecycle/` | The lifecycle-events feed on the internal port. Written in the change's transaction |
| `db/` | The v2 schema. Boot refuses a database with v1 migrations: v2 is a hard cutover |

Boot: wait for the first config, create the sign-in node if there is none, then listen. Each new config kicks the sweeper, which also runs every minute: nodes of a kind the config dropped are soft-deleted, a kind applied again gets its nodes back, nodes past `purge_after` are purged, and expired bindings are deleted.

## Tests

```bash
docker run -d --rm --name identity-pg -e POSTGRES_PASSWORD=test -p 55999:5432 postgres:17
TEST_DATABASE_URL=postgres://postgres:test@localhost:55999/postgres go test ./...
```

Without `TEST_DATABASE_URL`, tests that need Postgres are skipped.

## Local

```bash
cd ../../compose && docker compose up --build identity
```
