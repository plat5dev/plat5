# To do

## Next

- **Siblings follow the contracts.** Each moves to the shapes in `docs/`:
  - `toolbox`: its e2e and helpers use user routes, bare ids and `Bearer` keys, apply a config first since nothing is seeded, and read `node.*` events.
  - The docs site: syncs `reference/config/services.md`, `resources.md`, `settings.md` and `rate-limits.md`, which replace `reference/config/routes.md` and `reference/api/edge-catalog.md`; and the new sections on access outside a request (`guides/write-a-service.md`, `concepts/access.md`, `reference/api/internal.md`), at the release that ships them. The same for the two event feeds (`concepts/lifecycle.md`, `reference/api/internal.md`, `guides/write-a-service.md`), and the product page's mention of `/internal/lifecycle-events` moves to `/internal/tree-events`. The same for a person's name: `reference/idp.md` (the JWT access token contract, `users.name`), `reference/config/settings.md` and the `me` resource's `PATCH /user`. The same for route checks: `reference/gateway.md`, `reference/config/settings.md` (`route_checks`), `concepts/access.md` and `guides/build-a-console.md`.
  - The consoles: the customer console's profile page offers a name edit (`PATCH /user`), shows `name: null` as no name yet, and shows the **409** message when the IdP owns names. The operator console can set or clear a person's name, once `operator`'s route files add `PATCH` to `/users/{user_id}`, sent without `X-Plat5-User-Id`.
  - The API starters (`template-*`): each one's event consumer reads `/internal/tree-events`, and deletes the person's `/user/profile` on `user.deleted` from `/internal/user-events`, with its own cursor. They could also show checking access outside a request, for example a task notification that asks identity's evaluation API before it sends.
- **CI for the CLI.** Its schema-copy and init-files tests (`internal/schema`, `internal/cmd`) skip unless a `plat5` checkout sits next to the CLI, so CI must check one out there.

# Deferred

## Record-level access (AuthZEN below nodes)

Permissions on individual records inside a node ("may this member edit document D in this project?"). Who can reach a node is already Plat5's, and services already ask it: identity's evaluation API ([`docs/reference/api/internal.md`](docs/reference/api/internal.md#access-evaluation)). Record level is that API extended downward.

- **From a service.** The same endpoints, `POST /access/v1/evaluation` and `/access/v1/evaluations`, with a record as the resource (its type, its id, and the node it's in). Identity decides the node as it does now; an engine answers for the record after that. Without an engine, a record's answer is its node's.
- **A list, from a service.** The service asks which records a member can reach (AuthZEN's resource search). Without an engine, the answer is everything in the node.
- **One record, from the gateway.** Still deferred. An evaluation after the permission check: subject the member (never a principal id), action the route's permission, resource the service, the route and its own path params. Cached on the admission TTL. Engine unreachable → **503**.

Ready: a deployment that needs a record-level decision its service can't make as a state rule. No policy engine in the default stack.

## Subject search

Which members hold permission P on node N: AuthZEN's subject search (`POST /access/v1/search/subject`), on identity's internal port beside the evaluation API. For fan-out sends, such as notifying everyone who can read a project, without the service holding the list of members first. AuthZEN's resource and action searches stay out until record-level access needs them.

Ready: a service that needs it.

## Audit retention

Audit events are kept, including after a node is purged. Partitions are monthly, so retention is dropping old partitions. Identity's `node.purged` ([`docs/concepts/lifecycle.md`](docs/concepts/lifecycle.md#tree-events)) is the signal for erasing a purged tree's events.

Ready: storage growth that hurts, or an org whose events must be erased on purge.

## Lifecycle event retention

Identity's tree events and user events are kept. A consumer can always read either feed from its first event.

Ready: storage growth that hurts. Then prune by age, and answer a `starting_after` older than the oldest kept event with **410**, so the consumer knows to reconcile.

## Linking a second IdP subject

A person has one or more IdP links, and first sign-in creates the first. There is no API to add another. Ready: a person who signs in through a second IdP, or a deployment moving IdPs, and must stay one person.

## Per-tree custom roles

Every tree gets the deployment's roles. `GET /nodes/{node_id}/roles` already has the node in the path. Ready: a customer org that needs a role the deployment doesn't define.

## Group principals, public principals, cross-org moves, self-nesting kinds

The extension points in [`docs/concepts/boundaries.md`](docs/concepts/boundaries.md#extension-points). Ready: in order of likelihood, a customer who needs to share a project with a whole other org (group principals); a public link or "any signed-in user" grant (public principals); a project that must move to another org (cross-org move); access per folder in user-built folder trees (self-nesting kinds); the folders themselves are service data today.

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

## Service accounts across a subtree

`GET /nodes/{node_id}/service-accounts` lists those that belong to that node only. A filter (`?descendants=true`) would list those that belong to the node or any live node below it, each with its node's `chain`, filtered per node by `{kind}:bindings:read` like node lists. One place for an admin to see every machine in an org.

Ready: an org that needs to review every machine at once, for an access review or to find a leaked key's owner.
