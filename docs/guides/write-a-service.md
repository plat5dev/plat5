# Write a service

What a service behind Plat5 receives, what it should store, and what it must not do.

## What you receive

Requests arrive from a front door on the private network, with node ids in the path (or `X-Plat5-<Kind>-Id`), the caller's member id in `X-Plat5-Member-Id`, and `X-Plat5-Principal`. The full table: [`reference/headers.md`](../reference/headers.md). You never get a slug, a person's principal id, or the caller's labels or role.

Key your data by node id. Record a member id when logic depends on who did something, such as an author or an approver, and never decide access from it ([where a field goes](../concepts/model.md#where-a-field-goes)).

## Rules

The headers and rewritten path are authentic only if the service is on a private network that nothing but its front doors can reach.

1. **Trust the path and headers the front door wrote.** Read node ids from the path or `X-Plat5-<Kind>-Id`, the caller from `X-Plat5-Member-Id`.
2. **Document which headers you require.** A service that records authors says it needs `X-Plat5-Member-Id`, and answers **400** without it. Any front door that calls it supplies one.
3. **Never decide access by the caller.** Scope every lookup by the node id; enforce state rules. The front door already decided who may call.
4. **Propagate `traceparent`** on downstream calls.
5. **Log with `request_id`** from `X-Request-ID`. Don't set `X-Request-ID` on responses.
6. **Say what changed** with `X-Plat5-Audit-Details` on audited routes, and `X-Plat5-Audit-Created` on a create. No secrets ([`concepts/audit.md`](../concepts/audit.md#what-a-service-adds)).

## Platform integrity

1. **Do not parse `Authorization`** or read `X-API-Key`. You won't receive them.
2. **Do not validate JWTs.** The gateway validates via JWKS.
3. **Do not implement CORS.** The gateway handles preflight and headers.
4. **Do not generate `X-Request-ID`.** Propagate it. Missing is a gateway bug.

## Following the tree

Read identity's lifecycle feed to allocate state when a node is created, stop work when it's deleted, resume on restore, and delete data on purge. Remove member-owned records on `member.removed`. Events, and how to read them: [lifecycle](../concepts/lifecycle.md#lifecycle-events).

## Saying what changed

On audited routes, set `X-Plat5-Audit-Details` and, on a create, `X-Plat5-Audit-Created` ([audit](../concepts/audit.md#what-a-service-adds)).

## Publishing routes

Add a routes file for the service, list it in `plat5.yml`'s `services`, and apply ([routes](../reference/config/routes.md), [config](../concepts/config.md)).

## Direct-exposed services

Some services are exposed outside the gateway on purpose (e.g. an IdP at `auth.example.com` while the gateway serves `api.example.com`). They handle their own CORS, don't rely on subject headers, and are exempt from "Do not implement CORS". TLS still typically terminates at the edge.
