# Audit

Why audit events carry the node chain, live in one log per root and per person, and are read by node. The rules: [`concepts/audit.md`](../concepts/audit.md), [`reference/api/audit.md`](../reference/api/audit.md), [`internals/audit-storage.md`](../internals/audit-storage.md).

## What an event records

| Decision | Why |
|----------|-----|
| Every event records the node it touched and that node's ancestors | "This project" and "this org and everything under it" are both reads, with no second index of the tree |
| Audit is reporting only: product features never read it | A feature built on audit couples to its retention and its shape. Authorship belongs on the record ([access](access.md#what-services-see)) |
| Audit accepts what it can store, ignores unknown fields, and doesn't repeat other parts' rules | Otherwise a newer gateway or identity makes every audited request **503**. The gateway is the one place an event's fields are decided |
| Events record the key's id, not its display prefix | One id joins the event to the key list and the gateway's request log |
| Audit has no retention. Partitions are monthly, so retention, when it comes, is dropping old partitions ([`TODO.md`](../../TODO.md)) | Nothing has asked for it yet, and `node.purged` is the signal for erasing a purged tree's events |

## When it's written

| Decision | Why |
|----------|-----|
| The intent is written after the permission check | During an audit outage a **503** before the check would say a node exists to someone who can't see it |
| The gateway's own **403**s, **400**s and **500**s after admit are one background event, `rejected` | A refusal is worth recording, and nothing waits on it |

## Logs

| Decision | Why |
|----------|-----|
| An event is in the log of every node in its chain | Each tree is a closed world, and an org's admins read their org's history and nothing else |
| A node's log includes the event whose `created_id` is that node | A node's history starts with its own creation |
| A user route's service names the chain it changed with `X-Plat5-Audit-Nodes`, and the person's member there with `X-Plat5-Audit-Member`; the event joins every named node's log | The path of a user route names no tree. Naming the chain, not only the root, puts a project-level invite redeem in the project's log |
| A user route's event is in the person's log, keyed by their user id; in a named node's read it shows the named member, never the user id | The user id is never sent to a tree |
| Storage is one log-key table, one row per log an event is in | Every read is the same index range, whether the log is a node, a named node or a person |

## Reading by node

| Decision | Why |
|----------|-----|
| Each kind gets an audit route from audit's resources, instead of a `?node=` filter | The gateway can check permissions only at the route's node; a filter would let an org-level permission read below, or a project-level one read above |
| Reading is `audit:read`, with no kind in the name, checked at the node or above | A binding's reach is where it sits and below, so a role bound at a project already delegates that project's history without the org's. Two kinds of tree that need different audit readers use two roles |
| `GET /user/audit-events` is the caller's own user-route events, with no permission | A person can always see what they did with their own account |
| Reads filter by response status | "What failed" is the first question in an incident |
| Audit's test serves every audit upstream in the expanded-routes fixture with the ids the gateway fills, and checks what each read returns | Permissions are decided by admit and the gateway, so they're tested where admit lives. Audit only has to serve the paths |
| The e2e has its own audit test, skipped when `AUDIT_ENABLED=false`; the access flow's test runs unchanged with audit on | It adds what only audit can show (logs per node, the person's log, refusals) without coupling the two |
