# Lifecycle

Why deletes are soft, restorable and then purged, and how nodes, members and stored data begin and end. The rules: [`concepts/lifecycle.md`](../concepts/lifecycle.md), [`internals/identity-schema.md`](../internals/identity-schema.md).

## Deleting, restoring and purging

Every delete is soft for `deletion_grace`, 30 days by default, restorable, then purged, as a Google Cloud project is ([delete and restore](https://docs.cloud.google.com/resource-manager/docs/delete-restore-projects)).

| Decision | Why |
|----------|-----|
| A deleted node is refused by the gateway at once; its descendants cascade | Nothing reaches a node its owner deleted, while its data can still come back |
| Three events: `<kind>.deleted` (services stop serving, keep data), `<kind>.restored` (they resume), `<kind>.purged` (they delete data). Background jobs check node state; the gateway only guards requests | A service's data follows the node without a second source of truth |
| Restore is self-serve within the grace, by anyone who could delete; deleted nodes are listed with `?state=deleted` | The person who made the mistake can undo it |
| Keys and invites on a deleted node stay revoked after restore; nodes, bindings and service accounts come back | A key or token that leaked near the deletion stays dead |
| A deleted node's grace is fixed when it's deleted (`purge_after`), so changing `deletion_grace` applies to new deletes only | Shortening it must not purge what people were promised could be restored |
| After the grace, purged. Ids and slugs are never reused; the tombstone answers what used to exist | Google Cloud never reuses a project ID |
| Each delete or kind removal has a `deletion_id`; restore and a kind's re-apply undo only that id's nodes. It's a bare ULID | A node deleted separately earlier must stay deleted |
| Deleting an already deleted or purged node succeeds and changes nothing; it keeps its `deletion_id` and `purge_after`. The sign-in node can't be deleted (**422**) | Kubernetes accepts a delete of something already being deleted. A second delete mustn't extend the grace |
| Restoring a live node succeeds and changes nothing. Restoring under a deleted parent is **409**; parents are never restored for it. Restoring a node whose kind isn't in the config is **409**: apply the kind again | Google Cloud makes you undelete the parent first |
| Nothing is created under a deleted or purged parent (**409**). `PATCH`, bindings, service accounts and invites on a deleted node have no rule in the service API | The gateway already refuses deleted nodes; other front doors are trusted |
| Purge takes the deleted nodes below it, even if their own `purge_after` is later | A child under a purged parent can never be restored, so keeping it loses nothing but storage |
| A service account born at a purged node goes with it: principal and keys deleted, member removed with `member.removed` | Google Cloud deletes a project's service accounts with the project |
| A node's delete revokes the invites on it and below | As with keys, a token that leaked near a deletion stays dead |
| Delete answers **204**, restore **200** with the node, and releasing the slug of a node that isn't purged is **409** | Plain, and consistent with the rest of the API |
| The purge sweeper runs every minute and whenever a new config arrives. The binding sweeper runs with it, and marks invites past `expires_at` `expired`. No settings | One background loop; no setting until someone needs one |

## Removing a kind

| Decision | Why |
|----------|-----|
| Removing a kind soft-deletes its nodes and their descendants under one `deletion_id`, recorded in `kind_removals`. A returning kind restores them within grace; the row is dropped on restore or once that removal's nodes are purged | Re-creating a Kubernetes CRD starts empty; Plat5 makes it restorable for the grace. Dropping at purge means a kind applied again after the grace starts empty |
| Identity checks every config it reads against its nodes | A revision identity missed still takes effect: the controller pattern |

## Moving nodes

| Decision | Why |
|----------|-----|
| A node keeps its parent for life. A move within one tree is deferred ([`TODO.md`](../../TODO.md)) | When it comes, inherited roles switch to the new parent, as Google Cloud does when a project moves |
| Moving across trees is refused | The org is the isolation boundary ([boundaries](boundaries.md)) |

## Members

| Decision | Why |
|----------|-----|
| A member is created when a principal first joins a tree, and kept after removal | Records authored by a member resolve to a name for as long as they exist |
| A person's member left with no bindings, by a binding delete, expiry or purge, is removed, a guest included. A soft delete removes nobody | Their last way into the tree is gone. A soft-deleted node keeps its bindings until purge, so a restore brings back who had access |
| A service account's member stays until the service account is deleted, even with no bindings | Otherwise its principal and keys are orphaned |
| Deleting a service account deletes its principal and keys and removes its member, as at purge. Removing its member through the members route is **422** | One way for a service account to end |
| Lifecycle events are `<kind>.created/.deleted/.restored/.purged` and `member.created/.removed` | Services follow nodes and members without polling, and one shape fits every kind |

## Stored data

| Decision | Why |
|----------|-----|
| Each service's schema is one migration, `001_init.sql`. Boot refuses a database that records a migration the build doesn't know | Nothing has run against an older schema, so there's no upgrade path to keep. Data from elsewhere moves into a fresh database by hand |
| Identity's code keeps only what its schema uses: key hashing and format, the error envelope, pagination, ids, audit helpers, db, metrics, middleware, telemetry | Code with no schema behind it passes only against fakes. Git history keeps the rest |
