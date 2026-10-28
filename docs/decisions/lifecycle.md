# Lifecycle

Why deletes are soft, restorable and then purged, how nodes, members and stored data begin and end, and why services follow them in two feeds. The rules: [`concepts/lifecycle.md`](../concepts/lifecycle.md), [`internals/identity-schema.md`](../internals/identity-schema.md).

## Deleting, restoring and purging

Every delete is soft for `deletion_grace`, 30 days by default, restorable, then purged. A mistake can be undone for the whole grace, and the data is gone for good once it ends.

| Decision | Why |
|----------|-----|
| A deleted node is refused by the gateway at once; its descendants cascade | Nothing reaches a node its owner deleted, while its data can still come back |
| A delete revokes nothing. Restore is a full undo: nodes, bindings, service accounts, keys and unexpired invites. Purge deletes everything | Deleting a project by mistake and restoring it must not kill every CI key in it. Delete isn't a key kill switch: a leaked key is revoked directly, or its service account deleted |
| Redeem refuses an invite whose node isn't live | Nothing works on a deleted node, invites included |
| Four events: `node.deleted` (services stop serving, keep data), `node.restored` (they resume), `node.purged` (they delete data), and `node.created`, each with the node's `kind` | The set of types is fixed whatever kinds a deployment declares, so a service matches a type, not a suffix |
| Restore is self-serve within the grace, by anyone who could delete; deleted nodes are listed with `?status=deleted` | The person who made the mistake can undo it |
| A deleted node's grace is fixed when it's deleted (`purge_after`), so changing `deletion_grace` applies to new deletes only. The node carries `purge_after` | Shortening it must not purge what people were promised could be restored, and a console can show "restorable until" |
| Each delete has a `deletion_id`; restore undoes only that id's nodes. It's a bare ULID | A node deleted separately earlier must stay deleted |
| Deleting an already deleted or purged node succeeds and changes nothing; it keeps its `deletion_id` and `purge_after` | A client retrying a delete gets success, not an error for something that already happened. A second delete mustn't extend the grace |
| Restoring a live node succeeds and changes nothing. Restoring under a deleted parent is **409**; parents are never restored for it | A live node already is what the restore asks for. Restoring the parent is a separate decision with its own `deletion_id`, and a child's restore mustn't bring back what someone else deleted |
| Nothing is created under a deleted or purged parent (**409**). `PATCH`, bindings, service accounts and invites on a deleted node have no rule in the service API | The gateway already refuses deleted nodes; other front doors are trusted |
| Purge takes the deleted nodes below it, even if their own `purge_after` is later | A child under a purged parent can never be restored, so keeping it loses nothing but storage |
| A service account that belongs to a purged node goes with it: principal and keys deleted, member removed with `member.removed` | It belongs to that part of the tree. Left behind, its keys would authenticate a machine with nothing left to work on |
| Delete answers **204**, restore **200** with the node, and releasing the slug of a node that isn't a purged root is **409** | Plain, and consistent with the rest of the API |
| The purge sweeper runs every minute, and marks invites past `expires_at` `expired`. No settings | One background loop; no setting until someone needs one |

## Kinds

| Decision | Why |
|----------|-----|
| A kind is removed, or changes parent, only while it has no nodes that aren't purged, deleted ones included, so identity never deletes or moves nodes because of a config | A config change that soft-deletes data needs a confirmation flow, a record of what each removal deleted, and a rule for restoring it when the kind returns, possibly under other parents |
| A deleted node blocks its kind's removal until it's purged | Every deleted node stays restorable for its whole grace, and a restore always puts it where its kind can be, so restore needs no rule for a kind that's gone or moved |

## Moving nodes

| Decision | Why |
|----------|-----|
| A node keeps its parent for life. A move within one tree is deferred ([`TODO.md`](../../TODO.md)) | When it comes, inherited roles switch to the new parent: a node's access always follows the chain it's in |
| Moving across trees is refused | The org is the isolation boundary ([boundaries](boundaries.md)) |

## Members

| Decision | Why |
|----------|-----|
| A member is created when a principal first joins a tree, and kept after removal | Records authored by a member resolve to a name for as long as they exist |
| A person's member left with no bindings, by a binding delete or a purge, is removed, a guest included, and re-adding makes a new member. A soft delete removes nobody | Their last way into the tree is gone. A soft-deleted node keeps its bindings, so a restore brings back who had access. Changing a role is a `PATCH`, so a member keeps its id while it holds any binding |
| A service account's member stays until the service account is deleted, even with no bindings | Otherwise its principal and keys are orphaned |
| Deleting a service account deletes its principal and keys and removes its member, as at purge. Removing its member through the members route is **422** | One way for a service account to end |

## Lifecycle events

| Decision | Why |
|----------|-----|
| Two feeds, each with its own sequence: tree events (`node.*`, `member.*`) for tree data, user events (`user.*`) for user-route data | The same line as `X-Plat5-Member-Id` and `X-Plat5-User-Id`. A service that keeps only user-route data reads user events and never sees the tree. No sequence is shared, so no event's position links a user to a member, as one sequence would for an admin add's `user.created` beside its `member.created` |
| A user event carries only `id`, `type`, `user_id` and `occurred_at`. A tree event never carries a user id | A service deletes a person's user-route data with the user id alone, and nothing in either feed joins a person's data to a tree |
| `user.created` and `user.deleted` only. Suspension isn't an event | Suspension is access. A service asks about access through the evaluation API when its work runs, as it does for every other change in access, instead of following it |
| The events of one transaction share `occurred_at`, so a service reading both feeds can match them: an account deletion's `member.removed` events and its `user.deleted`, or an admin add's `user.created` and `member.created` | A service can already match an account deletion's `member.removed` events across trees that way, and services are trusted with it. The feeds are on identity's internal port, read only by the deployment's own services |

## Stored data

| Decision | Why |
|----------|-----|
| Each service's schema is one migration, `001_init.sql`. Boot refuses a database that records a migration the build doesn't know | One file shows the whole schema, and a build never runs against a schema it doesn't understand. Data from another database moves into a fresh one by hand |
| Identity's code keeps only what its schema uses: key hashing and format, the error envelope, pagination, ids, audit helpers, db, metrics, middleware, telemetry | Code with no schema behind it passes only against fakes |
