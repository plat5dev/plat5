# Deferred

## Gateway policy-engine hook (AuthZEN)

An optional AuthZEN 1.0 evaluation call from the gateway, after the label check, for decisions that need more than route labels. Subject is the member (`member_id` and `organization_id`, no `user_id`). Action is the route label. Resource is the service, the route, and its path params. Cached on the admission TTL. Engine unreachable → **503**.

Ready: a deployment that needs a resource-level decision its service cannot make. Until then, resource authz stays in the service. No policy engine in the default stack.

## Roles without a restart

Identity reads the roles file at boot. Ready: a deployment that changes roles often enough that a restart hurts. Then an internal admin endpoint, applied the way routes are.

## Per-org custom roles

Every org gets the deployment's roles. `GET /organizations/{organization_id}/roles` already has the org in the path. Ready: a customer org that needs a role the deployment does not define.
