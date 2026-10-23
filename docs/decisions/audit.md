# Audit

Why audit events carry the node chain, live in one log per root, and are read by node. The rules: [`concepts/audit.md`](../concepts/audit.md), [`reference/api/audit.md`](../reference/api/audit.md), [`internals/audit-storage.md`](../internals/audit-storage.md).

## What an event records

| Decision | Why |
|----------|-----|
| Every event records the node it touched and that node's ancestors | "This project" and "this org and everything under it" are both filters, with no second index of the tree |
| Audit is reporting only: product features never read it | A feature built on audit couples to its retention and its shape. Authorship belongs on the record ([access](access.md#what-services-see)) |
| An intent's `root_id` is `nodes[0].id`, `null` exactly when `nodes` is `[]`; `key_prefix` is set exactly for `apikey`; a service account has no `jwt` and no sign-in event. Anything else is **422**, and check constraints hold the same in the table | Each is a row that can't describe one request. The gateway never sends one, so a refusal is a bug caught early |
| A named root and member on a kind route's outcome are ignored, not **422** | The event's log is fixed by its chain. Refusing would leave it `pending` forever over a field that changes nothing |
| The chain is stored twice: `nodes` as sent, and `node_ids` (`text[]`) for the GIN index | Reads return the chain with kinds; the index only needs ids, and an array containment is the plainest GIN query |
| Audit has no retention. Partitions are monthly, so retention, when it comes, is dropping old partitions ([`TODO.md`](../../TODO.md)) | Nothing has asked for it yet, and `<kind>.purged` is the signal for erasing a purged tree's events |

## Logs per root

| Decision | Why |
|----------|-----|
| An event goes in the log of the root that owns it | Each tree is a closed world, and an org's admins read their org's history and nothing else |
| A sign-in route's service names the tree it changed with `X-Plat5-Audit-Root` and `X-Plat5-Audit-Member` | The path of a sign-in route names no tree |
| An event on a sign-in route has `root_id: null`; the person's log is keyed by `actor.member_id` | Admit returns no nodes on a sign-in route, so the gateway never knows the sign-in node's id, and the person's directory member already names the log |
| In a root's read, a sign-in event the service named shows the named member as `actor.member_id`, and `actor_member_id` filters on it; the person's log keeps the directory member | The directory member id is never sent to other trees |

## Reading by node

| Decision | Why |
|----------|-----|
| Each kind gets a generated audit route, instead of a `?node=` filter | The gateway can check labels only at the route's node; a filter would let an org-level label read below, or a project-level one read above |
| Reading stays behind `<kind>:audit:read` at that node or above | A project's own history can be delegated without the org's |
| Audit's test serves every audit upstream in the catalog fixture with the ids the gateway fills, and checks what each read returns; identity's label test also walks the audit routes | Labels are decided by admit and the gateway, so they're tested where admit lives. Audit only has to serve the paths |
| The e2e has its own audit test, skipped when `AUDIT_ENABLED=false`; the access flow's test runs unchanged with audit on | It adds what only audit can show (logs per root, reads by node, the person's log, label refusals) without coupling the two |
