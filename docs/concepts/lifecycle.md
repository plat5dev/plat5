# Lifecycle

How nodes and members are born, deleted, restored and purged, and the events services read to follow along.

## Creating nodes

Nodes are created through the generated node routes ([`reference/api/edge-catalog.md`](../reference/api/edge-catalog.md)), or identity's service API by another front door. The creator gets `creator_role` on the new node ([access](access.md#assigning)). A slug is fixed at creation ([ids and slugs](model.md#ids-and-slugs)). Identity writes `<kind>.created`. Nothing is created under a deleted or purged parent (**409**).

The sign-in node is created at boot, once identity has its first config, labelled with its kind's `label`, or the kind name if it has none.

A service never creates a node in its own tables first: it creates it through the node API, so the gateway's check never runs ahead of Plat5.

## Deleting, restoring and purging

- **Soft deletes.** Soft for `deletion_grace` (30 days by default), restorable, then purged. The grace is fixed when the node is deleted (`purge_after`), so changing `deletion_grace` applies to new deletes only. Ids are never reused. Slugs aren't either, unless someone releases a purged node's slug by hand through identity's service API, which no generated route exposes.

- **Deletion ids.** Each delete, and each kind removal, stamps one `deletion_id` on every node it soft-deletes. A restore, or a kind applied again, restores exactly the nodes with that id. A node already deleted keeps its own id.

- **Deleting again.** Deleting a node that's already deleted or purged succeeds and changes nothing: it keeps its first `deletion_id` and `purge_after`, so a second delete doesn't extend the grace. The sign-in node can't be deleted (**422**).

- **What a delete revokes.** Keys of service accounts born at the node or below, and invites on the node or below.

- **What a deleted node grants.** A node with `status = 'deleted'` grants nothing on it or below, except on a `resolve_deleted` route (restore, listing deleted). Once its `purge_after` has passed, the purge sweeper sets `status = 'purged'` on it and the deleted nodes below it, deletes bindings on them, deletes service accounts born at them (principal and keys; the member is removed with `member.removed`), removes members of a purged root, and keeps the rows.

- **Restore.** Before `purge_after`, by anyone who could delete the node: the node and the descendants its delete cascaded to come back. Keys and invites revoked by the delete stay revoked, so a key or token that leaked near the deletion stays dead. A node whose parent is deleted can't be restored until the parent is (**409**); parents are never restored for it. Restoring a live node succeeds and changes nothing. A node whose kind isn't in the config can't be restored (**409**): apply the kind again.

- **Background work** checks node state itself; the gateway only guards requests.


## Removing a kind

Removing a kind is the only destructive change. Identity soft-deletes its nodes when the new config reaches it, through the normal node lifecycle, so the same events are emitted. Applying the kind again within the grace period restores the nodes that removal deleted (not ones deleted separately before it), with `<kind>.restored` events; after it, the kind comes back empty.

Identity checks every config it reads against its nodes, so a removal it missed while down still happens. Live nodes of a kind the config doesn't have are soft-deleted, with their descendants, under one `deletion_id` that identity records for the kind. When the kind is in the config again, identity restores that removal's nodes still within their grace, except those whose parent has been deleted since, and forgets the removal. It also forgets it once all of that removal's nodes are purged, so the kind applied again after the grace starts empty. How the plan treats it: [config](config.md#classification).


## Members

- **Removed is final.** Re-adding someone to a tree makes a new member id. The old member stays, and a lookup returns its name and `status: removed`.

- **Members.** One active member per principal per tree. In an org's tree it's created with its first binding; in the directory, with the person: at first sign-in, or when an admin adds someone who hasn't signed in yet. Removal sets `removed_at`, deletes its bindings and emits `member.removed`; it's never undone, and re-adding makes a new member. A person's member outside the directory left with no bindings, by delete or expiry, is removed. A service account's member is removed only with the service account (deleted directly, or with its born node: either way its principal and keys are deleted); with no bindings its keys still authenticate and are refused everywhere, so a team can re-bind it.

- **Expiry.** A binding past `expires_at` grants nothing; a sweeper deletes it.

- **Suspension.** A suspended principal's credentials are all invalid. There's no per-member suspension: remove the member instead.


### Deleting an account

`DELETE /members/{member_id}` on a directory member, in one transaction: every member of the person in every tree is removed (bindings deleted, `member.removed` per tree), personal keys and IdP links are deleted, and the principal is deleted. Removed member rows stay, with no name. Nothing blocks it: an org left with no admin is recovered through an operator plane.

## Lifecycle events

A feed of births and ends other services may act on. Written in the same transaction as the change; a cascade writes one event per node, parents before children. Reading is opt-in. Plat5 names no queue or broker; anyone who wants push delivery runs a relay that reads this feed.

```
GET /internal/lifecycle-events?starting_after=<id>&limit=<n>
{ "lifecycle_events": [
  { "id": "812", "type": "project.deleted", "node_id": "n_…", "root_id": "n_…", "occurred_at": "…" },
  { "id": "813", "type": "member.removed", "member_id": "m_…", "root_id": "n_…", "principal": "person", "occurred_at": "…" }
], "has_more": false }
```

| Event | When | A service should |
|-------|------|------------------|
| `<kind>.created` | A node is created | Allocate state if it wants to before the first request |
| `<kind>.deleted` | Soft delete, directly or by cascade | Stop background work; keep the data |
| `<kind>.restored` | Restore within the grace period | Resume |
| `<kind>.purged` | `deletion_grace` after delete | Delete the data |
| `member.created` | A principal joins a tree: first sign-in, a redeemed invite, an admin add, a service account create, a root create | Nothing usually |
| `member.removed` | Removed, left, account deleted, a person left with no bindings, a service account deleted, or its root purged | Remove records owned by that member that should go with it |

`principal` is `person` or `service_account`. No principal id, name or role. Binding changes aren't events: they change access, which is the gateway's concern, not service data.

`id` is a decimal string from a sequence; event writes are serialized, so `id` order is commit order. Pass it back as `starting_after`; don't do arithmetic on it. Delivery is at least once from the consumer's side, so acting on an event must be idempotent. Events are kept ([`TODO.md`](../../TODO.md)).
