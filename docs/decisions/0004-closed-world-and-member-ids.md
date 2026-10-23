# 0004: Each tree is a closed world; services see member ids

- **Status:** Accepted. Replaces v1's "services see owners, never actors"
- **Date:** Oct 2026

v1 never told services who called, which made authorship impossible and kept operators from acting as anyone. v2 sends the caller, but only as a per-tree member id, never a person's id.

Locked. **Data inside a tree refers only to things inside it: its nodes and its members. People cross between trees; data doesn't.** No tree's data names another tree or a person's principal id. Plat5 guarantees the second part by never sending person ids; the first is a documented convention for services.

Three rules follow:

- **Authored acts are owned records**, recorded with the member id of whoever made them.
- **Unauthored acts are fine.** Who did them lives in audit, outside service data.
- **Crossing a boundary takes a binding**, which means appearing on the root's access list, as a member or a guest.

**Caller headers are allowed; person ids aren't.** Every request Plat5's gateway admits carries the node ids, the caller's member id (`X-Plat5-Member-Id`) and `X-Plat5-Principal`. The member id is per tree, so two orgs can't join their data on a shared person. Gateways with one identity system per app, such as Kong, Google's Identity-Aware Proxy and Cloudflare Access, pass the caller through too; Plat5 differs only in never passing a cross-tree id. Other front doors, like the operator plane, call the same service APIs under their own documented rules. Access is still decided only at the front door, never by a service reading the caller.

### Stress test

| Case | Holds? | How it works | Verdict |
| --- | --- | --- | --- |
| "Published by Dana" on a version | Yes | The version is owned by Dana's member id | Fine |
| CI publishes the catalog from the repo | Yes | The CI service account is a binding in the org | Fine |
| "Don't notify me about my own change" | Yes | The change is an owned record, so its owner is known | Fine |
| Notification preferences per org or project | Yes, and better | Keyed by the person's member id in that org; leaving the org deletes them | The rule improves the design |
| Support grants an override for a customer | Strains | Support must be a member of the customer's org | Fine once bindings can expire |
| Operators fix data during an incident | Yes | Node routes; no author on the record; the operator audit log has the operator | Fine |
| One inbox across all of a person's orgs | Strains | The client asks each org and merges, or entries are copied in as plain values with no ids | Needs a stated pattern |
| An agency managing many client orgs | Breaks | Agency people join each org one by one; "org A manages org B" can't be expressed | Later: group principals (extension points) |
| "Last edited by" on every row | Strains | Each edit becomes a revision record, or the column goes | Cost accepted |

### Additions this needs

- **Bindings that expire.** Support access becomes "the customer adds a support member for 7 days": visible, consented to, and self-ending. Whether support members exist at all is the deployment's roles: a `support` role with a `max_duration`. There's no per-org setting.
- **A stated pattern for views across trees.** Built by the client from one credential per tree, or copied in as values with no ids. Never references.

The agency case is answered later by group principals: the agency org is bound into each client project or org, so no ids cross trees. One outside person on one project is already covered in v2 by guest bindings.
