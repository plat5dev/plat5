# Write a service

What a service behind Plat5 receives, what it should store, what it must not do, how it keeps tenants apart, how it adds its own fields to a node, and how it checks access outside a request.

## What you receive

Requests arrive from a front door on the private network. On a tree route: node ids in the path (or `X-Plat5-<Kind>-Id`), the caller's member id in `X-Plat5-Member-Id`, and `X-Plat5-Principal`. On a user route: the caller's user id in the path (or `X-Plat5-User-Id`). The full table: [`reference/headers.md`](../reference/headers.md). You never get a slug, a principal id, or the caller's permissions or role.

Key your data by node id. Record a member id when logic depends on who did something, such as an author or an approver, and never decide access from it ([where a field goes](../concepts/model.md#where-a-field-goes)). Key a person's own data, the same in every tree, by user id, and serve it on user routes.

## Rules

The headers and rewritten path are authentic only if the service is on a private network that nothing but its front doors can reach.

1. **Trust the path and headers the front door wrote.** Read node ids from the path or `X-Plat5-<Kind>-Id`, the caller from `X-Plat5-Member-Id`.
2. **Document which headers you require.** A service that records authors says it needs `X-Plat5-Member-Id`, and answers **400** without it. Any front door that calls it supplies one.
3. **Never decide access by the caller.** Scope every lookup by the node id; enforce state rules. The front door already decided who may call. For anyone else, or outside a request, ask identity ([below](#checking-access-outside-a-request)).
4. **Propagate `traceparent`** on downstream calls.
5. **Log with `request_id`** from `X-Request-ID`. Don't set `X-Request-ID` on responses.
6. **Say what changed** with `X-Plat5-Audit-Details` on audited routes, and `X-Plat5-Audit-Created` on a create. No secrets ([`concepts/audit.md`](../concepts/audit.md#what-a-service-adds)).

## Platform integrity

1. **Do not parse `Authorization`.** You won't receive it.
2. **Do not validate JWTs.** The gateway validates via JWKS.
3. **Do not implement CORS.** The gateway handles preflight and headers.
4. **Do not generate `X-Request-ID`.** Propagate it. Missing is a gateway bug.

## Keep tenants apart

Every read and write inside a node is scoped by the node id from the request, never by a record id alone. The record id comes from the client; the node id comes from the front door, which decided the caller may act on that node. A query by record id alone serves another tenant's record to anyone who has its id.

```sql
-- Wrong: any tenant's task, for whoever sends its id
SELECT * FROM tasks WHERE id = $2;

-- Right: the task only if it's in the environment the request is about
SELECT * FROM tasks WHERE env_id = $1 AND id = $2;
```

`$1` is the node id from the path or `X-Plat5-Environment-Id`, `$2` the record id the client sent. A record in another node is the same **404** as one that doesn't exist. Updates and deletes take the same `WHERE`, and an insert takes the node id from the request, never from the body.

Optionally, store ancestor ids beside the node id, from `X-Plat5-<Kind>-Id` (an `org_id` from `X-Plat5-Organization-Id`): for roll-ups across a level, such as every task in an org, or as a second layer with Postgres row-level security keyed on them. They never go stale, because a node keeps its parent for life. They add to the scope from the request; they never replace it.

## Following the tree and people

Identity writes two feeds ([lifecycle](../concepts/lifecycle.md#lifecycle-events)). Read the one for the data you keep; a service that keeps both reads both, with a cursor for each.

**Tree data**, keyed by node and member ids, follows `GET /internal/tree-events`. Allocate state on `node.created` if you want it before the first request. Stop background work on `node.deleted` and keep the data; resume on `node.restored`. Delete the node's data on `node.purged`, matching on the event's `kind`. On `member.removed`, delete the records that member owned; authored records stay ([where a field goes](../concepts/model.md#where-a-field-goes)). `member.created` needs nothing.

**User-route data**, keyed by user id, follows `GET /internal/user-events`. On `user.deleted`, delete everything keyed by that user id. `user.created` usually needs nothing: create a person's data the first time they call a user route.

An event can arrive more than once, so handling it twice must be harmless. Shapes and paging: [internal APIs](../reference/api/internal.md#tree-events).

## Nodes with service fields

A node is identity's: its id, kind, parent, slug, display name and status. Anything else about it, such as a project's region or plan, is the service's, keyed by the node id ([where a field goes](../concepts/model.md#where-a-field-goes)).

**The default is two requests.** Identity creates the node through its own route. Then the console sends the service's fields to the service's route for that node with `PUT`, which is safe to repeat. The service treats a node with no settings yet as having the defaults, creating its row lazily on the first request or on `node.created`. The console sends both from one form ([build a console](build-a-console.md#nodes-with-service-fields)).

**When a field must exist from the start**, the service owns the create:

1. The deployment turns identity's create off for that kind, and gives the collection path to the service's route:

   ```yaml
   # services/identity.yml
   identity:
     url: http://identity:3000
     kinds:
       project:
         resources:
           children:
             methods:
               POST: false
   ```

   The service's route is `POST /projects` in its `organization` scope (`POST /orgs/{organization}/projects`), requiring what identity's did, `project:create` by default ([resources](../reference/config/resources.md)). `GET` on the collection stays identity's.
2. The service validates its fields first. Then it creates the node through identity's service API, `POST /nodes/{parent id}/children/project` with `{ display_name, slug? }`, passing the caller's `X-Plat5-Member-Id`, so the creator is bound when the kind binds its creator ([identity's API](../reference/api/identity.md)).
3. It inserts its own row. If that fails, it deletes the node through identity (`DELETE /nodes/{node id}`) and answers with its error.
4. It sets `X-Plat5-Audit-Created` to the new node's id ([audit](../concepts/audit.md#what-a-service-adds)).

A node deleted in step 3 is a deleted node like any other until its purge, after `deletion_grace`: it holds its slug, so a retry with the same slug is a **409**, and it's listed with `?status=deleted`. Take an idempotency key from the client, such as an `Idempotency-Key` header (listed in `cors.allowed_headers` for browsers), so a retry after a timeout answers the node already made instead of making a second.

If that node is restored, it still has no row. The service treats the missing row as not set up: its other routes for the node answer **409** until the console sends the fields with `PUT`.

For a root kind, the resource is `roots` (`kinds.<kind>.resources.roots`), the service's route is a `user` route, and identity's create is `POST /roots/{kind}`, passing the caller's `X-Plat5-User-Id`. A user route names the tree it changed with `X-Plat5-Audit-Nodes` and `X-Plat5-Audit-Member`.

## Checking access outside a request

The front door decides for the caller, during the request. When your service acts for a member at another time (a notification, a digest, a scheduled report, an outbound webhook, a queued job), or on behalf of a member who isn't the caller (an assignee, a mention, a share), ask identity whether that member holds the permission on the node. A member can lose a node while staying in the tree, and no event says so ([access](../concepts/access.md#outside-a-request)).

Ask identity's internal port, from your own config's identity internal URL ([evaluation](../reference/api/internal.md#access-evaluation)). Ask when the work runs, not when it's queued.

**Before a send.** One batch for everyone you'd notify about a node; send only to the `true`s:

```
POST {identity internal URL}/access/v1/evaluations
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

Up to 100 items a call; split a longer list.

**Before accepting an assignment.** The caller is allowed to assign; ask whether the assignee may work on the node:

```
POST {identity internal URL}/access/v1/evaluation
{
  "subject": { "type": "member", "id": "<assignee's member id>" },
  "resource": { "type": "project", "id": "<project id>" },
  "action": { "name": "<permission from your config>" }
}
→ { "decision": false }
```

`false` is your own refusal, such as a **422** naming the assignee.

**Permission names come from your config.** The permissions belong to the deployment: its roles grant them ([roles](../reference/config/roles.md)). Read the names you check from your service's own config, not from code, so the deployment can set them. A permission the deployment renames without your config following answers `false` for everyone: the check fails closed.

**No answer is no.** On anything but a **200**, don't send; retry later.

**Checking is your job.** Plat5 can't enforce it: it never sees what a service sends, or to whom. A service that skips the check gets no error, only a leak.

## Saying what changed

On audited routes, set `X-Plat5-Audit-Details` and, on a create, `X-Plat5-Audit-Created` ([audit](../concepts/audit.md#what-a-service-adds)).

## Publishing routes

Add a service file for the service, list it in `plat5.yml`'s `services`, grant its permissions in `roles`, and apply ([services](../reference/config/services.md), [config](../concepts/config.md)). `plat5 routes list` shows the public paths.

## Direct-exposed services

Some services are exposed outside the gateway on purpose (e.g. an IdP at `auth.example.com` while the gateway serves `api.example.com`). They handle their own CORS, don't rely on the gateway's headers, and are exempt from "Do not implement CORS". TLS still typically terminates at the edge.
