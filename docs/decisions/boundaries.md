# Boundaries

Why each tree is a closed world, what Plat5 is for, and how it grows to sharing beyond one org without changing the tree. The rules: [`concepts/boundaries.md`](../concepts/boundaries.md), [`concepts/model.md`](../concepts/model.md#principles).

## What Plat5 is for

Plat5 is for a stable tenancy boundary enforced outside every handler: the tree is fixed by config, and who can be bound to it is the axis that grows.

| Decision | Why |
|----------|-----|
| For hierarchical B2B products whose tenancy boundary is a small, stable set of kinds (org, project, maybe environment). Nodes come and go freely by API; the kinds rarely change | That's where one tree, checked once at the gateway, saves every service from writing its own |
| Not for permissions on individual records inside a node, or hierarchies end users invent and reshape | A flat org plus the product's own tables is lighter there, or the deferred policy hook below nodes ([`TODO.md`](../../TODO.md)) |
| One writer of the tree. A service that needs nodes creates them through the node API | Two writers would let the gateway's check run ahead of Plat5's own records |

## Each tree is a closed world

**Data inside a tree refers only to things inside it: its nodes and its members. People cross between trees; data doesn't.** No tree's data names another tree, a principal id or a user id. Plat5 guarantees the second part by never sending principal ids, and sending user ids only on user routes; the first is a documented convention for services.

| Decision | Why |
|----------|-----|
| Authored acts are owned records, with the member id of whoever made them; unauthored acts are fine, and who did them lives in audit | Authorship stays in the tree it happened in |
| Crossing a boundary takes a binding, which means appearing on the root's access list, as a member or a guest | Everyone who can reach a tree's data is visible to its admins |
| Views across trees are built by the client from one credential per tree, or copied in as plain values with no ids. Never references | A reference across trees is a join on a person or a node another org owns |

How the rule holds up:

| Case | How it works | Verdict |
|------|--------------|---------|
| "Published by Dana" on a version | The version is owned by Dana's member id | Holds |
| CI publishes from the repo | The CI service account is a binding in the org | Holds |
| "Don't notify me about my own change" | The change is an owned record, so its owner is known | Holds |
| Notification preferences per org or project | Keyed by the person's member id in that org; leaving the org deletes them | Holds, and better |
| A profile or setting the same in every org | Keyed by the user id, on user routes only | Holds |
| Support grants an override for a customer | Support is a member of the customer's org, through a binding the customer adds and removes | Holds |
| Operators fix data during an incident | Node routes; no author on the record; the operator plane's audit log has the operator | Holds |
| One inbox across all of a person's orgs | The client asks each org and merges, or entries are copied in as values with no ids | Holds, with the stated pattern |
| An agency managing many client orgs | Agency people join each org one by one; "org A manages org B" can't be expressed | Later: group principals |
| "Last edited by" on every row | Each edit becomes a revision record, or the column goes | Cost accepted |

## Sharing beyond one org

Google Cloud's role bindings name users, service accounts, groups, whole domains, `allAuthenticatedUsers` and `allUsers` ([IAM binding](https://docs.cloud.google.com/dotnet/docs/reference/Google.Cloud.Iam.V1/latest/Google.Cloud.Iam.V1.Binding)). Plat5 grows the same way: sharing is a question of principals, not of the tree.

| Case | Today | Extension |
|------|-------|-----------|
| One outside person on one project | Guest bindings: an invite below the org, gated by `guests:write`, listed on the org's members list | — |
| A whole org shared into a project; an agency across client orgs | Not possible | **Group principals:** a binding whose principal is "members of org A", like Google Cloud's `group:` and `domain:`. Open point: authored records need a per-person binding, for example one created the first time someone acts through the group |
| Anyone with a link, or any signed-in user | Not possible | **Public principals,** like `allUsers` and `allAuthenticatedUsers` |
| Moving a project to another org | Refused | **Cross-org move** with a `node.moved` event; records authored before the move keep their old member ids, which resolve as former members |
| Data in one org referring to data in another | Blocked by design | None. Share the container instead: bind people into one project |

None of these touches nodes, the ancestor rule, slugs or what services see; they add principal types and change how admit unions bindings.

## Other extension points

| Extension | Shape |
|-----------|-------|
| Permissions below a node | The deferred policy hook ([`TODO.md`](../../TODO.md)) |
| A kind nested under itself (user folders) | A kind allowed as its own parent with a depth cap, nodes past the first level addressed by id. Google Cloud folders nest the same way |
| Per-tree custom roles | `GET /nodes/{node_id}/roles` already has the node in the path |
| A request-and-approve flow for support access | Like Google Cloud's Access Approval ([Access Approval](https://docs.cloud.google.com/assured-workloads/access-approval/docs/approve-requests)), on top of bindings |
| A kind optional in the middle | `parent` as a list again, which is additive. One parent per kind keeps one chain, and one public path, per route |

## Costs accepted

| Cost | Why it's accepted |
|------|-------------------|
| Adopting Plat5 means putting the tree under it: routes, path params and "whose data is this" line up with the kinds. It isn't an SDK drop-in | That alignment is what lets the gateway decide access once |
| Moving off Plat5 is a migration of nodes and bindings, not an auth-provider swap | The tree is data Plat5 owns |
| Services see a per-tree member id on tree routes, and a user id only on user routes | Keeping person ids out of tree data is the one property that's hard to reverse, and it's intended: a person can't be correlated across orgs from what an org's routes see |
