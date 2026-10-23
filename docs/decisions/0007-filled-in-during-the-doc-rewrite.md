# 0007: Details filled in while writing the v2 contracts

- **Status:** Accepted
- **Date:** Oct 2026

Splitting the spec into contracts, and the review after it, turned up gaps the spec didn't answer. Each was decided as below; the contracts hold the rules.

## Decisions

| Gap | Decision | Why |
|-----|----------|-----|
| A node list can't be filtered by the list route's own labels: a project-only member has no `project:read` at the org and would get 403 | List routes are `[]`; identity filters each node by the labels of that kind's read route | Google Cloud's project search returns what you have `projects.get` on |
| Restoring a deleted node, especially a root with nothing above it | `resolve_deleted` on the restore route; on it, the deleted node's own bindings, kept until purge, still count | Google Cloud keeps a deleted project's policy so it can be undeleted |
| A caller outside a tree could spend that tree's rate-limit budget | Not found and not in the tree are a **403** before the rate limit, on the failed-auth IP limiter; a missing label is a **403** after it. Same body | Keeps budgets per tree without leaking existence |
| Identity's own API had no shape | Everything by id: `/nodes/{id}/…`, `/roots/{kind}`, `/members/{id}/…`. A service account is addressed by its member id | Any front door can call it without slugs |
| Members had no add, leave or lookup routes | Add by IdP subject, remove, leave (`DELETE /orgs/{organization}/me`), lookup by id with `[]` labels | Consoles must show "Dana (former member)" to anyone who sees her records |
| Which tree a sign-in route changed | `X-Plat5-Audit-Root` and `X-Plat5-Audit-Member` from the service | The path of a sign-in route names no tree |
| Reading a project's audit | A generated audit route per kind, instead of a `?node=` filter | The gateway can only check labels at the route's node |
| Restore and kind re-apply undoing the right nodes | A `deletion_id` per delete or kind removal | A node deleted separately earlier must stay deleted |
| A service account whose last binding expires | Its member stays until the service account is deleted | Otherwise its principal and keys are orphaned |
| Time-limited roles at join | `expires_at` on member add; `role_duration` on invites; `default_role` and `creator_role` have no `max_duration` | A `max_duration` role was otherwise impossible to grant at join |
| Smaller wiring | `route_prefix` on the service; sign-in routes rate-limit per directory member; `AUTH_SUBJECT_CLAIM`, `ADMIT_URL`, `IDENTITY_INTERNAL_URL` | Scope blocks are gone; the sign-in node is one node for everyone |
