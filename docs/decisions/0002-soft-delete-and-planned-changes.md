# 0002: Soft deletes and planned config changes

- **Status:** Accepted
- **Date:** Oct 2026

Config is data that can delete data: removing a kind removes its nodes. The question was whether to forbid that, as some systems do, or allow it with safeguards.

## Changing the config and deleting things

Destructive changes are allowed when the caller has seen the plan and confirmed it, and every deletion is soft for a configurable grace period, 30 days by default. This takes Kubernetes' rule that a kind can be deleted with its objects ([Kubernetes](https://v1-36.docs.kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/_print/)) and runs it through Google Cloud's rule that a deleted project is restorable for 30 days and its ID never reused ([Google Cloud](https://docs.cloud.google.com/resource-manager/docs/delete-restore-projects)).

### Applying a config change

Every apply produces a plan first, machine-readable so CI can run it.

- **Safe:** applies.
- **Blocked:** refused, listing the nodes or bindings in the way.
- **Destructive:** applies only with a confirmation naming each affected kind.

Tightened rules (a shorter `max_duration`, a new reserved slug) apply to new values only, as Kubernetes validation ratcheting does ([KEP-4008](https://www.kubernetes.dev/resources/keps/4008/)).

| Change | Class |
| --- | --- |
| Add a kind at the bottom (`environment` under `project`) | Safe |
| Add a kind in the middle (`folder` between org and project) | Safe, in two steps: add `folder` under `organization`, then widen `project`'s parent to `[organization, folder]` |
| Widen a kind's allowed parents | Safe |
| Narrow a kind's allowed parents | Blocked while any node sits under the parent being removed |
| Remove a kind | Destructive. Cascades to its nodes and their descendants; kinds that name it as parent must go or be re-parented in the same apply |
| Rename a kind | Not supported; change `label` instead |
| Change `label`, add a reserved slug | Safe |
| Remove `bindable_at` kinds from a role, or remove a role | Safe. Bindings keep the role's name and grant nothing until it's back, as with a deleted Google Cloud custom role |


### Deleting nodes

- **Soft for deletion\_grace, 30 days by default.** The gateway refuses the node at once. Children cascade.
- **Three events:** `<kind>.deleted` (services stop serving, keep data), `<kind>.restored` (services resume), `<kind>.purged` (services delete data). Background jobs check node state; the gateway only guards requests.
- **Self-serve restore** within the grace period by anyone who could delete it; deleted nodes are listed with `?state=deleted`. A deleted kind comes back by re-applying it, an operator action.
- **Keys stay revoked.** Nodes, bindings and service accounts come back; keys don't, so one that leaked near the deletion stays dead.
- **After the grace period, purged.** Ids and slugs are never reused; the tombstone lets operators answer what used to exist.

### Moving nodes

- **Within an org** (between folders, once a middle kind exists): allowed. Inherited roles switch to the new parent, as Google Cloud does when a project moves ([Resource Manager](https://docs.cloud.google.com/resource-manager/docs/resource-manager-overview)).
- **Across orgs:** refused. The org is the isolation boundary.
