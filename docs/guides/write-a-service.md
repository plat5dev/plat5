# Write a service

A service behind Plat5 is a plain HTTP server in any language. This guide separates the few rules required for security and isolation from the optional Plat5 standards you can follow for richer platform capabilities.

## What you receive

Requests arrive from a front door on the private network:

- **Tree routes:** resolved node ids in the path (and in `X-Plat5-<Kind>-Id`), `X-Plat5-Member-Id` (omitted on anonymous or non-member `allow_public` requests), and `X-Plat5-Principal` (`person`, `service_account`, or `anonymous`).
- **User routes:** the caller's user id in the path (and in `X-Plat5-User-Id`), and `X-Plat5-Principal`.
- **Every route:** `X-Request-ID`, `traceparent`, and (when `UPSTREAM_SIGNING_KEY` is set) `X-Plat5-Signature`.

Full header list: [`reference/headers.md`](../reference/headers.md). Services never receive slugs, `Authorization`, principal ids, or the caller's role or permissions.

## Must do

These three rules keep multi-tenant isolation intact:

1. **Stay on the private network — or verify `X-Plat5-Signature`.** The rewritten path and `X-Plat5-*` headers are authentic only if nothing except your front doors (Plat5's gateway, an operator plane, etc.) can reach the service, or the gateway signs requests with `UPSTREAM_SIGNING_KEY` and your service verifies `X-Plat5-Signature` ([`reference/headers.md`](../reference/headers.md#upstream-request-signing)).
2. **Scope every query by the node id from the request.** Never look up, update, or delete a record by its record id alone — the record id comes from the client, while the node id comes from the front door.
   ```sql
   -- Wrong: serves another tenant's task to anyone with its id
   SELECT * FROM tasks WHERE id = $2;

   -- Right: scoped to the project the gateway authorized
   SELECT * FROM tasks WHERE project_id = $1 AND id = $2;
   ```
   A record in another node returns the same **404** as one that doesn't exist. Inserts take the node id from the request, never from the body. (You may also store ancestor ids from `X-Plat5-<Kind>-Id` alongside the node id for roll-ups or Postgres RLS, never instead of it.)
3. **Leave auth and access decisions to the front door.** Don't validate JWTs, parse `Authorization`, implement CORS, or check roles yourself. Document which `X-Plat5-*` headers your endpoints require (e.g. `X-Plat5-Member-Id` when recording an author) and answer **400** if a required header is missing. On routes with `allow_public: true`, treat `X-Plat5-Member-Id` as optional and handle `X-Plat5-Principal: anonymous`.

Key tree data by node id (and `X-Plat5-Member-Id` when logic needs an author or owner). Key per-person data that is the same across every tree by `X-Plat5-User-Id` on user routes ([where a field goes](../concepts/model.md#where-a-field-goes)).

## Follow Plat5 standards for more

Everything below is opt-in — each gives you a concrete capability across the platform:

### Audit log enrichment
Set optional response headers on audited routes ([`concepts/audit.md`](../concepts/audit.md#what-a-service-adds)) to get richer customer audit logs:
- `X-Plat5-Audit-Details` (a JSON object, e.g. `{ "status": { "from": "open", "to": "done" } }`) → shows before/after diffs in the audit log.
- `X-Plat5-Audit-Created` → records the created resource's id on the audit event.
- `X-Plat5-Audit-Nodes` and `X-Plat5-Audit-Member` (on user routes) → also publishes the event into the affected tree's audit logs.

The gateway strips all `X-Plat5-Audit-*` headers before responding to the client. Never put secrets in details.

### End-to-end traces and correlated logs
- **Log `X-Request-ID` as `request_id`** → tie a single request across gateway logs, audit events, error responses, and your service logs.
- **Propagate `traceparent`** (via any OpenTelemetry SDK) → get one distributed trace from the edge through the gateway into your handlers and database ([`reference/telemetry.md`](../reference/telemetry.md)).

### Consistent client errors and pagination
- **Follow the [error envelope](../reference/errors.md)** (`error: { type, code, message, request_id, details }`, using whatever `code` values fit your domain) → clients need only one error parser across gateway, identity, audit, and your APIs.
- **Follow [cursor pagination](../reference/lists.md)** (`limit`, `starting_after`, `has_more`) → one list pagination pattern across your whole API.

### Automatic cleanup via the lifecycle log
Follow identity's transactional lifecycle log (`GET /internal/events`, [`reference/api/internal.md`](../reference/api/internal.md#lifecycle-events)) to clean up your own tables when data ends ([`concepts/lifecycle.md`](../concepts/lifecycle.md#following-the-log)):
- `node.purged` → delete the node's data (matching on `kind`).
- `member.removed` → delete member-owned data (subscriptions, preferences); authored records stay.
- `user.deleted` → delete everything keyed by that `user_id`.

Keep your cursor in the same database as the data it cleans up, make handlers idempotent, and restart from the first event on **409** `CURSOR_AHEAD`.

### Checking access outside a request
The gateway checks access for the caller during a request. When your service acts outside a request (notifications, digests, scheduled jobs, webhooks) or on behalf of another member (assignees, mentions), ask identity's internal AuthZEN endpoint ([`reference/api/internal.md`](../reference/api/internal.md#access-evaluation)) when the work runs:

```json
POST {IDENTITY_INTERNAL_URL}/access/v1/evaluations
{
  "resource": { "type": "project", "id": "<project id>" },
  "action": { "name": "<permission from your config>" },
  "evaluations": [
    { "subject": { "type": "member", "id": "<member id>" } },
    { "subject": { "type": "member", "id": "<another member id>" } }
  ]
}
→ { "evaluations": [ { "decision": true }, { "decision": false } ] }
```

Read permission names from your service's own config rather than hardcoding them, and fail closed on any non-200 response (don't send; retry later).

For cross-node queries inside a broader scope (for example, listing a member's tasks across every project they can read in an org), ask identity's resource search endpoint ([`reference/api/internal.md`](../reference/api/internal.md#resource-search)):

```json
POST {IDENTITY_INTERNAL_URL}/access/v1/search/resource
{
  "subject":  { "type": "member", "id": "<member id>" },
  "action":   { "name": "<permission from your config>" },
  "resource": { "type": "project" },
  "scope":    { "node_id": "<organization id>" }
}
→ { "all": true, "results": [] }
// or { "all": false, "results": [ { "type": "project", "id": "<project id>" } ] }
```

When `all` is `true`, the member holds the permission on every live node of that kind under `scope`; otherwise filter by the returned `results` ids.

### Record-level auth with an external PDP
If your routes attach an external AuthZEN 1.0 authorizer with `resource: { type, id }` ([authorizers](../reference/config/authorizers.md)), the gateway enforces record-level decisions before requests reach your handler. Leave `cache_ttl` and `negative_cache_ttl` at `0s` (the default) when share/unshare writes must take effect immediately, or use short TTLs (`2s`–`5s`) for high-traffic read routes. See the [Topaz playbook](../playbooks/record-level-auth-topaz.md).

## Nodes with service fields

Identity stores a node's `id`, `kind`, `parent_id`, `slug`, `display_name`, and `status`. Any product-specific fields about a node (like a project's region or billing settings) live in your service, keyed by the node id:

- Identity creates the node through its route.
- Your service treats a node with no row yet as having defaults, creating its row lazily on first read/write or via a follow-up `PUT` from the console.
