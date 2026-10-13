# Deferred

## Gateway policy-engine hook (AuthZEN)

An optional AuthZEN 1.0 evaluation call from the gateway, after the label check, for decisions that need more than route labels. Subject is the member (`member_id` and `organization_id`, no `user_id`). Action is the route label. Resource is the service, the route, and its path params. Cached on the admission TTL. Engine unreachable → **503**.

Ready: a deployment that needs a resource-level decision its service cannot make. Until then, resource authz stays in the service. No policy engine in the default stack.

## Client IP behind a proxy

`client_ip()` trusts the leftmost `X-Forwarded-For` entry from anyone. Cloudflare appends to a client-sent `X-Forwarded-For` rather than replacing it, so a client can choose the IP on an audit event and the `public` rate-limit bucket. Behind one trusted hop, take the rightmost entry or `CF-Connecting-IP`. In general, trusted proxy CIDRs and walk from the right.

Ready: before an audit event's `ip` is used as evidence, or before `public` rate limits matter.

## Audit retention

Audit events are kept, including after an org is deleted. Partitions are monthly, so retention is dropping old partitions. Identity's `organization.deleted` ([`docs/identity.md`](docs/identity.md#lifecycle-events)) is the signal for erasing a deleted org's events.

Ready: storage growth that hurts, or an org whose events must be erased on delete.

## Lifecycle event retention

Identity's lifecycle events are kept. A consumer can always read from the first event.

Ready: storage growth that hurts. Then prune by age, and answer a `starting_after` older than the oldest kept event with **410**, so the consumer knows to reconcile.

## Organization existence check

A batch internal lookup (`organization_id`s in, the missing ones out) for work a service does outside a request, such as a digest or an outbound webhook. It answers whether the org still exists when the work runs, without waiting on the feed.

Ready: a service that does work outside a request for an org that may be gone.

## Audit events for `user` routes

Creating an org, redeeming an invite, and minting a session are in no org log, because a `user` route has no verified org. A service could name the org on `X-Plat5-Audit-Details` (it is trusted, the caller is not), and the gateway could log into it.

Ready: an org that needs join or session-mint events in its log.

## Roles without a restart

Identity reads the roles file at boot. Ready: a deployment that changes roles often enough that a restart hurts. Then an internal admin endpoint, applied the way routes are.

## Per-org custom roles

Every org gets the deployment's roles. `GET /organizations/{organization_id}/roles` already has the org in the path. Ready: a customer org that needs a role the deployment does not define.
