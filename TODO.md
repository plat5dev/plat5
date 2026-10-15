# Deferred

## Gateway policy-engine hook (AuthZEN)

Two calls to an optional policy engine ([`docs/model.md`](docs/model.md#what-i-can-reach)):

- **One resource, from the gateway.** An AuthZEN 1.0 evaluation after the label check. Subject is the member (`member_id` and `organization_id`, no `user_id`). Action is the route label. Resource is the service, the route, and its path params. Cached on the admission TTL. Engine unreachable → **503**.
- **A list, from a service.** On a `member` route, the service asks which resources its subject can reach (an AuthZEN search, or the engine's lookup). The subject is the route's `member_id`, never the caller. Without an engine, the answer is everything in the org.

Ready: a deployment that needs a resource-level decision its service cannot make. Until then, resource authz stays in the service. No policy engine in the default stack.

## Audit retention

Audit events are kept, including after an org is deleted. Partitions are monthly, so retention is dropping old partitions. Identity's `organization.deleted` ([`docs/identity.md`](docs/identity.md#lifecycle-events)) is the signal for erasing a deleted org's events.

Ready: storage growth that hurts, or an org whose events must be erased on delete.

## Lifecycle event retention

Identity's lifecycle events are kept. A consumer can always read from the first event.

Ready: storage growth that hurts. Then prune by age, and answer a `starting_after` older than the oldest kept event with **410**, so the consumer knows to reconcile.

## Organization existence check

A batch internal lookup (`organization_id`s in, the missing ones out) for work a service does outside a request, such as a digest or an outbound webhook. It answers whether the org still exists when the work runs, without waiting on the feed.

Ready: a service that does work outside a request for an org that may be gone.

## Linking a second IdP subject

A person has one or more IdP links, and first sign-in creates the first. There is no API to add another. Ready: a person who signs in through a second IdP, or a deployment moving IdPs, and must stay one person.

## Per-org custom roles

Every org gets the deployment's roles. `GET /organizations/{organization_id}/roles` already has the org in the path. Ready: a customer org that needs a role the deployment does not define.

## Operator console extension

A custom-pages slot, or a page that browses a service's `operator.routes`. Today staff see a service's data only through the raw Request page. Ready: a deployment whose support work lives in its own service's data.

## Operator routes that merge

`operator.routes` replaces the image's route list, so image updates no longer reach it. Merge a deployment's routes with the image's. Ready: a deployment that adds staff routes and takes a release.

## Customer web app as source

The demo web app is a prebuilt image (`template-web`). Put its source in the project, or `plat5 eject web`. Ready: newcomers who need to change the demo, not only the API.

## Audit event by request id

Every error envelope gives the caller a `request_id`, but audit reads are per realm, so staff need the org or person first. A staff-only read by `request_id` across realms, on the operator gateway only. An event in both a person's log and an org's comes back twice. Ready: support that works from tickets carrying a `request_id`.
