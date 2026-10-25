# Lifecycle

Why deletes are soft, restorable and then purged, and how nodes, members and stored data begin and end. The rules: [`concepts/lifecycle.md`](../concepts/lifecycle.md), [`internals/identity-schema.md`](../internals/identity-schema.md).

## Deleting, restoring and purging

Every delete is soft for `deletion_grace`, 30 days by default, restorable, then purged, as a Google Cloud project is ([delete and restore](https://docs.cloud.google.com/resource-manager/docs/delete-restore-projects)).

| Decision | Why |
|----------|-----|
| A deleted node is refused by the gateway at once; its descendants cascade | Nothing reaches a node its owner deleted, while its data can still come back |
| A delete revokes nothing. Restore is a full undo: nodes, bindings, service accounts, keys and unexpired invites. Purge deletes everything | Deleting a project by mistake and restoring it must not kill every CI key in it. Delete isn't a key kill switch: a leaked key is revoked directly, or its service account deleted |
| Redeem refuses an invite whose node isn't live | Nothing works on a deleted node, invites included |
| Four events: `node.deleted` (services stop serving, keep data), `node.restored` (they resume), `node.purged` (they delete data), and `node.created`, each with the node's `kind` | The set of types is fixed whatever kinds a deployment declares, so a service matches a type, not a suffix |
| Restore is self-serve within the grace, by anyone who could delete; deleted nodes are listed with `?status=deleted` | The person who made the mistake can undo it |
| A deleted node's grace is fixed when it's deleted (`purge_after`), so changing `deletion_grace` applies to new deletes only. The node carries `purge_after` | Shortening it must not purge what people were promised could be restored, and a console can show "restorable until" |
| Each delete has a `deletion_id`; restore undoes only that id's nodes. It's a bare ULID | A node deleted separately earlier must stay deleted |
| Deleting an already deleted or purged node succeeds and changes nothing; it keeps its `deletion_id` and `purge_after` | Kubernetes accepts a delete of something already being deleted. A second delete mustn't extend the grace |
| Restoring a live node succeeds and changes nothing. Restoring under a deleted parent is **409**; parents are never restored for it. Restoring a node whose kind isn't in the config, or whose parent's kind isn't its kind's parent, is **409** | Google Cloud makes you undelete the parent first. A restore must never put a node where its kind can't be |
| Nothing is created under a deleted or purged parent (**409**). `PATCH`, bindings, service accounts and invites on a deleted node have no rule in the service API | The gateway already refuses deleted nodes; other front doors are trusted |
| Purge takes the deleted nodes below it, even if their own `purge_after` is later | A child under a purged parent can never be restored, so keeping it loses nothing but storage |
| A service account born at a purged node goes with it: principal and keys deleted, member removed with `member.removed` | Google Cloud deletes a project's service accounts with the project |
| Delete answers **204**, restore **200** with the node, and releasing the slug of a node that isn't a purged root is **409** | Plain, and consistent with the rest of the API |
| The purge sweeper runs every minute, and marks invites past `expires_at` `expired`. No settings | One background loop; no setting until someone needs one |

## Kinds

| Decision | Why |
|----------|-----|
| Only an empty kind can be removed, and a kind's parent changes only while it has no nodes, so identity never deletes or moves nodes because of a config | A config change that soft-deletes data needs a confirmation flow, a record of what each removal deleted, and a rule for restoring it when the kind returns, possibly under other parents. Google Cloud deletes only an empty folder |

## Moving nodes

| Decision | Why |
|----------|-----|
| A node keeps its parent for life. A move within one tree is deferred ([`TODO.md`](../../TODO.md)) | When it comes, inherited roles switch to the new parent, as Google Cloud does when a project moves |
| Moving across trees is refused | The org is the isolation boundary ([boundaries](boundaries.md)) |

## Members

| Decision | Why |
|----------|-----|
| A member is created when a principal first joins a tree, and kept after removal | Records authored by a member resolve to a name for as long as they exist |
| A person's member left with no bindings, by a binding delete or a purge, is removed, a guest included, and re-adding makes a new member. A soft delete removes nobody | Their last way into the tree is gone. A soft-deleted node keeps its bindings, so a restore brings back who had access. Changing a role is a `PATCH`, so a member keeps its id while it holds any binding |
| A service account's member stays until the service account is deleted, even with no bindings | Otherwise its principal and keys are orphaned |
| Deleting a service account deletes its principal and keys and removes its member, as at purge. Removing its member through the members route is **422** | One way for a service account to end |

## Stored data

| Decision | Why |
|----------|-----|
| Each service's schema is one migration, `001_init.sql`. Boot refuses a database that records a migration the build doesn't know | Nothing has run against an older schema, so there's no upgrade path to keep. Data from elsewhere moves into a fresh database by hand |
| Identity's code keeps only what its schema uses: key hashing and format, the error envelope, pagination, ids, audit helpers, db, metrics, middleware, telemetry | Code with no schema behind it passes only against fakes. Git history keeps the rest |
