# Lifecycle

How nodes and members are created, deleted, restored and purged, and the events services read to follow along.

## Creating nodes

Nodes are created through identity's routes ([resources](../reference/config/resources.md)), or identity's service API by another front door. The creator gets `creator_role` on the new node ([access](access.md#assigning)). A slug is fixed at creation ([ids and slugs](model.md#ids-and-slugs)). Identity writes `node.created`. Nothing is created under a deleted or purged parent (**409**).

A service never creates a node in its own tables first: it creates it through the node API, so the gateway's check never runs ahead of Plat5.

## Deleting, restoring and purging

- **Soft deletes.** Soft for `deletion_grace` (30 days by default), restorable, then purged. The grace is fixed when the node is deleted (`purge_after`), so changing `deletion_grace` applies to new deletes only. Ids are never reused.

- **Deletion ids.** Each delete stamps one `deletion_id` on every node it soft-deletes. A restore restores exactly the nodes with that id. A node already deleted keeps its own id.

- **Deleting again.** Deleting a node that's already deleted or purged succeeds and changes nothing: it keeps its first `deletion_id` and `purge_after`, so a second delete doesn't extend the grace.

- **A delete revokes nothing.** While a node is deleted, nothing works on it or below: the gateway refuses it, the keys of service accounts that belong there are refused, and its invites can't be redeemed. Bindings, service accounts, keys and invites are all kept. To kill a key you think leaked, revoke it or delete its service account.

- **What a deleted node grants.** Nothing on it or below, except on an `allow_deleted` route (restore). Listing deleted nodes counts its own bindings ([lists](access.md#lists)).

- **Restore.** Before `purge_after`, by anyone who could delete the node: a full undo. The node and the descendants its delete cascaded to come back, with their bindings, service accounts, keys and unexpired invites. A node whose parent is deleted can't be restored until the parent is (**409**); parents are never restored for it. Restoring a live node succeeds and changes nothing.

- **Purge.** Once `purge_after` has passed, the purge sweeper sets `status = 'purged'` on the node and the deleted nodes below it, even those whose own `purge_after` is later, and deletes everything that hung off them: bindings, invites, and service accounts that belong there (principal and keys; the member removed with `member.removed`). A person's member it leaves with no bindings, a guest included, is removed with `member.removed`, and a purged root removes all its members. The node rows are kept. A purged node below a root frees its slug; a purged root keeps its slug as a tombstone ([ids and slugs](model.md#ids-and-slugs)).

- **Background work** checks node state itself; the gateway only guards requests.

## Kinds

A kind is removed, or changes parent, only while it has no nodes, deleted ones included ([config](config.md#blocked-changes)), so a config change never deletes or moves a node, and every deleted node can be restored where it was. Identity follows a new kind at once: its nodes can be created as soon as the config reaches identity.

## Members

- **Removed is final.** Re-adding someone to a tree makes a new member id. The old member stays, and a lookup returns its name and `status: removed`.

- **Members.** One active member per principal per tree, created with its first binding. Removal sets `removed_at`, deletes its bindings and emits `member.removed`; it's never undone, and re-adding makes a new member. A person's member left with no bindings, by a binding delete or a purge, is removed. A service account's member is removed only with the service account (deleted directly, or at purge of the node it belongs to: either way its principal and keys are deleted); with no bindings its keys still authenticate and are refused everywhere, so a team can re-bind it.

- **Suspension.** A suspended person's credentials are all invalid. There's no per-member suspension: remove the member instead.

### Deleting an account

`DELETE /users/{user_id}`, in one transaction: every member of the person in every tree is removed (bindings deleted, `member.removed` per tree), personal keys and IdP links are deleted, and the principal is deleted. Removed member rows stay, with no name. Nothing blocks it: an org left with no admin is recovered through an operator plane.

## Lifecycle events

A feed of what's created and what ends, for other services to act on. Written in the same transaction as the change; a cascade writes one event per node, parents before children. Reading is opt-in. Plat5 names no queue or broker; anyone who wants push delivery runs a relay that reads this feed.

```
GET /internal/lifecycle-events?starting_after=<id>&limit=<n>
{ "lifecycle_events": [
  { "id": "812", "type": "node.deleted", "kind": "project", "node_id": "01jbx…", "root_id": "01jbw…", "occurred_at": "…" },
  { "id": "813", "type": "member.removed", "member_id": "01jby…", "root_id": "01jbw…", "principal": "person", "occurred_at": "…" }
], "has_more": false }
```

The set of types is fixed, whatever kinds a deployment declares. Node events carry the node's `kind`.

| Event | When | A service should |
|-------|------|------------------|
| `node.created` | A node is created | Allocate state if it wants to before the first request |
| `node.deleted` | Soft delete, directly or by cascade | Stop background work; keep the data |
| `node.restored` | Restore within the grace period | Resume |
| `node.purged` | `deletion_grace` after delete | Delete the data |
| `member.created` | A principal joins a tree: a redeemed invite, an admin add, a service account create, a root create | Nothing usually |
| `member.removed` | Removed, left, account deleted, a person left with no bindings, a service account deleted, or its root purged | Remove records owned by that member that should go with it |

`principal` is `person` or `service_account`. No principal id, user id, name or role. Binding changes aren't events: they change access, which is the gateway's concern, not service data.

`id` is a decimal string from a sequence; event writes are serialized, so `id` order is commit order. Pass it back as `starting_after`; don't do arithmetic on it. Delivery is at least once from the consumer's side, so acting on an event must be idempotent. Events are kept ([`TODO.md`](../../TODO.md)).
