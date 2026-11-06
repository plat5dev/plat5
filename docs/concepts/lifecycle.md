# Lifecycle

How nodes and members are created, deleted, restored and purged, how a person's account ends, and the events services read to follow along.

## Creating nodes

Nodes are created through identity's routes ([generated routes](../reference/config/generated-routes.md)), or identity's service API by another front door. The creator gets `creator_role` on the new node when its kind binds its creator ([access](access.md#assigning)). A slug is fixed at creation ([ids and slugs](model.md#ids-and-slugs)). Identity writes `node.created`. Nothing is created under a deleted or purged parent (**409**).

A service never creates a node in its own tables first: it creates it through the node API, so the gateway's check never runs ahead of Plat5.

## Deleting, restoring and purging

- **Soft deletes, for nodes only.** Deleting a service account, a binding or an account is final ([members](#members), [deleting an account](#deleting-an-account)). A node delete is soft for `deletion_grace` (30 days by default), restorable, then purged. The grace is fixed when the node is deleted (`purge_after`), so changing `deletion_grace` applies to new deletes only. Ids are never reused.

- **Deletion ids.** Each delete stamps one `deletion_id` on every node it soft-deletes. A restore restores exactly the nodes with that id. A node already deleted keeps its own id.

- **Deleting again.** Deleting a node that's already deleted or purged succeeds and changes nothing: it keeps its first `deletion_id` and `purge_after`, so a second delete doesn't extend the grace.

- **A delete revokes nothing.** While a node is deleted, nothing works on it or below: the gateway refuses it, the keys of service accounts that belong there are refused, and its invites can't be redeemed. Bindings, service accounts, keys and invites are all kept. To kill a key you think leaked, revoke it or delete its service account.

- **What a deleted node grants.** Nothing on it or below, except the node itself on an `allow_deleted` route (restore). Listing deleted nodes counts its own bindings ([lists](access.md#lists)). So through the gateway, restoring a node whose parent is deleted, or creating one under a deleted parent, is the usual **403**: the parent is in the chain. **409** `PARENT_DELETED` is identity's answer to other front doors.

- **Restore.** Before `purge_after`, by anyone who could delete the node: a full undo. The node and the descendants its delete cascaded to come back, with their bindings, service accounts, keys and unexpired invites. A node whose parent is deleted can't be restored until the parent is (**409**); parents are never restored for it. Restoring a live node succeeds and changes nothing.

- **Purge.** Once `purge_after` has passed, the purge sweeper sets `status = 'purged'` on the node and the deleted nodes below it, even those whose own `purge_after` is later, and deletes everything that hung off them: bindings, invites, and service accounts that belong there (principal and keys; the member removed with `member.removed`). A person's member it leaves with no bindings, a guest included, is removed with `member.removed`, and a purged root removes all its members. The node rows are kept. A purged node below a root frees its slug; a purged root keeps its slug as a tombstone ([ids and slugs](model.md#ids-and-slugs)).

- **A purged node is read-only.** Every write to it, or to what hangs off it, is **409** `NODE_PURGED`: renaming it, creating children under it, adding or changing its bindings, members and service accounts, and creating or revoking its service accounts' keys and its invites. Two writes are allowed: releasing a purged root's slug ([slug release](../reference/api/identity.md#slug-release)), and the deletes that find nothing left (the node, a binding, a member, a service account), which answer **204** and change nothing. A restore past the grace is **409** `GRACE_EXPIRED`, purged or not. A deleted node that isn't purged yet takes every write a live node does, since it can still be restored.

- **Background work** checks node state and access itself, through identity's evaluation API ([access](access.md#outside-a-request)); the gateway only guards requests.

## Kinds

A kind is removed, or changes parent, only while it has no nodes, deleted ones included ([config](config.md#blocked-changes)), so a config change never deletes or moves a node, and every deleted node can be restored where it was. Identity follows a new kind at once: its nodes can be created as soon as the config reaches identity.

## Members

- **Removed is final.** Re-adding someone to a tree makes a new member id. The old member stays, and a lookup returns its name and `status: removed`.

- **Members.** One active member per principal per tree, created with its first binding. Removal sets `removed_at`, deletes its bindings and emits `member.removed`; it's never undone, and re-adding makes a new member. A person's member left with no bindings, by a binding delete or a purge, is removed. A service account's member is removed only with the service account (deleted directly, or at purge of the node it belongs to: either way its principal and keys are deleted); with no bindings its keys still authenticate and are refused everywhere, so a team can re-bind it.

- **Suspension.** A suspended person's credentials are all invalid. There's no per-member suspension: remove the member instead.

### Deleting an account

`DELETE /users/{user_id}`, in one transaction: every member of the person in every tree is removed (bindings deleted, `member.removed` per tree), personal keys and IdP links are deleted, and the principal is deleted, with `user.deleted`. Removed member rows stay, with no name. Nothing blocks it: an org left with no admin is recovered through an operator plane. A join racing it (a redeem, an admin add, a root create) either lands first, and its member is removed with the rest, or acts as it would once the account is gone: a redeem or root create is refused as for a deleted user ([identity's errors](../reference/api/identity.md#errors)), and an admin add finds the subject new and creates the person.

## Lifecycle events

What's created and what ends, for other services to act on, in one log on identity's internal port. Each event is written in the same transaction as the change. Reading is opt-in. Plat5 names no queue or broker; anyone who wants push delivery runs a relay that reads the log. Shape, params and waiting: [internal APIs](../reference/api/internal.md#lifecycle-events).

| Event | When |
|-------|------|
| `node.created` | A node is created |
| `node.deleted` | Soft delete, directly or by cascade |
| `node.restored` | Restore within the grace period |
| `node.purged` | `deletion_grace` after delete |
| `member.created` | A principal joins a tree: a redeemed invite, an admin add, a service account create, a root create |
| `member.removed` | Removed, left, account deleted, a person left with no bindings, a service account deleted, or its root purged |
| `user.created` | A person comes to exist: first sign-in, or an admin adding someone who has never signed in |
| `user.deleted` | The account is deleted ([deleting an account](#deleting-an-account)) |

The set of types is fixed, whatever kinds a deployment declares. A cascade writes one event per node, parents before children. Node events carry the node's `kind`; member events carry `principal`, `person` or `service_account`.

Events hold the same line as `X-Plat5-Member-Id` and `X-Plat5-User-Id`: a node or member event never carries a user id, and a user event carries the person's user id and nothing else: no tree, no member, no name. No event carries a principal id or a role.

Binding changes and suspension aren't events: they're access, which is asked for, not pushed. A service asks identity when its work runs ([access](access.md#outside-a-request)).

### What the log guarantees

- **Commit order.** Ids are in commit order, and a reader never skips an event.
- **One transaction, together.** The events of one transaction are adjacent in the log and share `occurred_at`: an account deletion's `member.removed` events and its `user.deleted`, or an admin add's `user.created` and `member.created`.
- **Kept.** Every event is kept for the life of the deployment, so the log can always be read from its first event.
- **At least once.** From the consumer's side, an event can arrive more than once.

### Following the log

- **Read from the first event.** Keep a cursor, the last id acted on, and save it after acting.
- **Keep the cursor with the data it cleans up,** in the same database, so a restore from backup rewinds both together and the replay redoes exactly the deletes the backup is missing.
- **Make every handler safe to run twice.**
- **Replay to reconcile.** To recover a lost or restored database, or to fix a handler bug, move the cursor back, to the start if needed, and read again. There is no other reconcile.

Three events carry a duty, for a service that keeps the data they name:

| Event | The service |
|-------|-------------|
| `node.purged` | Deletes the node's data, matching on `kind` |
| `member.removed` | Deletes what that member owned; authored records stay ([where a field goes](model.md#where-a-field-goes)) |
| `user.deleted` | Deletes everything keyed by that user id |

Each is an ending that never changes, so a service that reads the log from its first event and acts idempotently ends up correct. The other five are informational. State is created lazily, and background work checks node state and access through identity's evaluation API when it runs ([access](access.md#outside-a-request)).
