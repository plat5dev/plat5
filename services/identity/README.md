# identity

Plat5 **identity**: the tree of nodes, and the principals, users, members, bindings, keys and invites in it.

Contracts: [identity API](../../docs/reference/api/identity.md), [internal APIs](../../docs/reference/api/internal.md), [lifecycle](../../docs/concepts/lifecycle.md), [schema](../../docs/internals/identity-schema.md). Its gateway routes are its [resources](../../docs/reference/config/resources.md), expanded by the control plane: nothing is generated.

## Layout

| Package | Role |
|---------|------|
| `feed/` | The config from the control plane's `GET /internal/identity`: kinds with their `list_permission`, roles with their includes resolved, `creator_role`, `deletion_grace`, `reserved_slugs`. Identity doesn't listen until it has one, and long-polls for changes |
| `nodes/` | Nodes, the closure table, ancestors on the node shape, slugs and root tombstones, soft delete, restore, purge, and the sweeper that runs purge. `POST /internal/plan` on the internal port: the control plane's counts for a plan that removes a kind or changes its parent |
| `members/` | People and their user ids and IdP links, members of each tree, bindings and the access list, memberships, service accounts, API keys, invites and redeem; creating nodes with the creator's binding, and filtered node lists. `POST /internal/admit` on the internal port: the member and permissions for a credential on a node chain. `POST /access/v1/evaluation` and `/access/v1/evaluations` there too: AuthZEN access evaluation for services, decided by admit's resolution |
| `lifecycle/` | The two event feeds on the internal port: `GET /internal/tree-events` (`node.*`, `member.*`) and `GET /internal/user-events` (`user.created`, `user.deleted`), each with its own sequence. Written in the change's transaction |
| `db/` | The schema, one migration. Boot refuses a database that records migrations this build doesn't know |

Boot: wait for the first config, then listen. The sweeper runs every minute: nodes past `purge_after` are purged, and invites past their expiry are marked expired.

## Tests

```bash
docker run -d --rm --name identity-pg -e POSTGRES_PASSWORD=test -p 55999:5432 postgres:17
TEST_DATABASE_URL='postgres://postgres:test@localhost:55999/postgres?sslmode=disable' go test ./...
```

Without `TEST_DATABASE_URL`, tests that need Postgres are skipped.

`members/catalog_test.go` walks every identity route the gateway serves the way the gateway sends it: the routes from the control plane's `fixtures/catalog/gateway-feed.json`, the config from `fixtures/catalog/identity-feed.json`. Regenerate those there when the resources change.

## Local

```bash
cd ../../compose && docker compose up --build identity
```
