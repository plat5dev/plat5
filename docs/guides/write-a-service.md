# Write a service

What a service behind Plat5 receives, what it should store, and what it must not do.

## What you receive

Requests arrive from a front door on the private network. On a tree route: node ids in the path (or `X-Plat5-<Kind>-Id`), the caller's member id in `X-Plat5-Member-Id`, and `X-Plat5-Principal`. On a user route: the caller's user id in the path (or `X-Plat5-User-Id`). The full table: [`reference/headers.md`](../reference/headers.md). You never get a slug, a principal id, or the caller's permissions or role.

Key your data by node id. Record a member id when logic depends on who did something, such as an author or an approver, and never decide access from it ([where a field goes](../concepts/model.md#where-a-field-goes)). Key a person's own data, the same in every tree, by user id, and serve it on user routes.

## Rules

The headers and rewritten path are authentic only if the service is on a private network that nothing but its front doors can reach.

1. **Trust the path and headers the front door wrote.** Read node ids from the path or `X-Plat5-<Kind>-Id`, the caller from `X-Plat5-Member-Id`.
2. **Document which headers you require.** A service that records authors says it needs `X-Plat5-Member-Id`, and answers **400** without it. Any front door that calls it supplies one.
3. **Never decide access by the caller.** Scope every lookup by the node id; enforce state rules. The front door already decided who may call.
4. **Propagate `traceparent`** on downstream calls.
5. **Log with `request_id`** from `X-Request-ID`. Don't set `X-Request-ID` on responses.
6. **Say what changed** with `X-Plat5-Audit-Details` on audited routes, and `X-Plat5-Audit-Created` on a create. No secrets ([`concepts/audit.md`](../concepts/audit.md#what-a-service-adds)).

## Platform integrity

1. **Do not parse `Authorization`.** You won't receive it.
2. **Do not validate JWTs.** The gateway validates via JWKS.
3. **Do not implement CORS.** The gateway handles preflight and headers.
4. **Do not generate `X-Request-ID`.** Propagate it. Missing is a gateway bug.

## Following the tree

Read identity's lifecycle feed to allocate state on `node.created`, stop work on `node.deleted`, resume on `node.restored`, and delete data on `node.purged`, matching on the event's `kind`. Remove member-owned records on `member.removed`. Events, and how to read them: [lifecycle](../concepts/lifecycle.md#lifecycle-events).

## Saying what changed

On audited routes, set `X-Plat5-Audit-Details` and, on a create, `X-Plat5-Audit-Created` ([audit](../concepts/audit.md#what-a-service-adds)).

## Publishing routes

Add a service file for the service, list it in `plat5.yml`'s `services`, grant its permissions in `roles`, and apply ([services](../reference/config/services.md), [config](../concepts/config.md)). `plat5 routes list` shows the public paths.

## Direct-exposed services

Some services are exposed outside the gateway on purpose (e.g. an IdP at `auth.example.com` while the gateway serves `api.example.com`). They handle their own CORS, don't rely on subject headers, and are exempt from "Do not implement CORS". TLS still typically terminates at the edge.
