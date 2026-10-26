# To do

## Next

- **Siblings follow the contracts.** Each moves to the shapes in `docs/`:
  - `toolbox`: its e2e and helpers use user routes, bare ids and `Bearer` keys, apply a config first since nothing is seeded, and read `node.*` events.
  - The docs site: syncs `reference/config/services.md`, `resources.md`, `settings.md` and `rate-limits.md`, which replace `reference/config/routes.md` and `reference/api/edge-catalog.md`.
- **CI for the CLI.** Its schema-copy and init-files tests (`internal/schema`, `internal/cmd`) skip unless a `plat5` checkout sits next to the CLI, so CI must check one out there.
- **A name in dev tokens.** Plat5 Auth's `POST /dev/token` takes a `name` and puts it in the token as the `name` claim, so `plat5 start`'s dev person gets a name. The CLI already sends it.

# Deferred

## Policy hook below nodes (AuthZEN)

Permissions on individual records inside a node ("may this member edit document D in this project?"). Who can reach a node is already Plat5's: the ancestor rule over bindings ([`docs/concepts/access.md`](docs/concepts/access.md#the-rule)). The hook is only for what's below that.

- **One record, from the gateway.** An AuthZEN 1.0 evaluation after the permission check. Subject is the member (`member_id` and the node chain, never a principal id). Action is the route's permission. Resource is the service, the route, and its own path params. Cached on the admission TTL. Engine unreachable → **503**.
- **A list, from a service.** The service asks which records the caller's member can reach (an AuthZEN search). Without an engine, the answer is everything in the node.

Ready: a deployment that needs a record-level decision its service can't make as a state rule. No policy engine in the default stack.

## Audit retention

Audit events are kept, including after a node is purged. Partitions are monthly, so retention is dropping old partitions. Identity's `node.purged` ([`docs/concepts/lifecycle.md`](docs/concepts/lifecycle.md#lifecycle-events)) is the signal for erasing a purged tree's events.

Ready: storage growth that hurts, or an org whose events must be erased on purge.

## Lifecycle event retention

Identity's lifecycle events are kept. A consumer can always read from the first event.

Ready: storage growth that hurts. Then prune by age, and answer a `starting_after` older than the oldest kept event with **410**, so the consumer knows to reconcile.

## Node existence check

A batch internal lookup (node ids in, the deleted or missing ones out) for work a service does outside a request, such as a digest or an outbound webhook. It answers whether the node is still live when the work runs, without waiting on the feed. `GET /nodes/{node_id}` answers one at a time today.

Ready: a service whose background work fans out over many nodes.

## Linking a second IdP subject

A person has one or more IdP links, and first sign-in creates the first. There is no API to add another. Ready: a person who signs in through a second IdP, or a deployment moving IdPs, and must stay one person.

## Per-tree custom roles

Every tree gets the deployment's roles. `GET /nodes/{node_id}/roles` already has the node in the path. Ready: a customer org that needs a role the deployment doesn't define.

## Group principals, public principals, cross-org moves, self-nesting kinds

The extension points in [`docs/concepts/boundaries.md`](docs/concepts/boundaries.md#extension-points). Ready: in order of likelihood, a customer who needs to share a project with a whole other org (group principals); a public link or "any signed-in user" grant (public principals); a project that must move to another org (cross-org move); user-built folder trees (self-nesting kinds).

## Moving a node within its tree

A node keeps its parent for life. There's no API to move one to another parent in the same tree. So a kind can't be added in the middle of a populated tree, and a kind's parent can't change while it has nodes ([config](docs/concepts/config.md#blocked-changes)). A move would rewrite the node's `parent_id` and its closure rows, keep its bindings, and emit an event so services can follow. It stays in one tree; moving across trees is the cross-org move above.

Ready: a deployment that adds or reshapes a middle kind with nodes under it. Then `parent` as a list (a project under an org or a folder) can come back, additively.

## Request-and-approve support access

Support access is a binding the customer adds and removes. A flow where support requests and the customer approves is later. Ready: a customer who requires approval per access.

## Audit event by request id

Every error envelope gives the caller a `request_id`, but audit reads are by node, so a front door needs the node first. A read by `request_id` across roots, for other front doors only (no default route). An event in both a person's log and a tree's comes back once. Ready: support that works from tickets carrying a `request_id`.

## Rollout status for identity

Gateway replicas report the revision they loaded on every long poll, and `plat5 apply` reports them ([`docs/reference/api/control-plane.md`](docs/reference/api/control-plane.md#reader-status)). Identity replicas don't report yet, and `plat5 apply` waits only briefly.

Ready: a deploy or a test that must know a roles change is live in identity everywhere before it continues, or a `plat5 apply --wait` that blocks until every replica has the revision.

## Binding end times

Bindings don't expire. An optional `expires_at` on a binding can be added later without breaking anything. Ready: a customer who needs access that ends by itself, with no one to remove it.
