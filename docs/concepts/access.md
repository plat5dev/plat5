# Access

Who can do what, in one place: the ancestor rule, who can be in a tree, roles and bindings, credentials, how the gateway decides a request, how a service checks access outside one, what lists show, and what a UI shows. The shapes these rules act on are in [reference](../README.md#reference).

## The rule

- **Ancestor rule (built-in RBAC).** When the deployment declares `roles` + `creator_role`, built-in RBAC allows a request when the caller holds one of the route's permissions through an unexpired binding on the route's node or an ancestor. Built-in access is the union of those bindings; there are no built-in deny rules.

- **External AuthZEN authorizer (all-or-nothing PDP mode).** A deployment may instead declare a single top-level `authorizer` ([`reference/config/authorizer.md`](../reference/config/authorizer.md)) pointing to an external OpenID AuthZEN 1.0 PDP (such as Topaz, Cerbos, or OpenFGA). `authorizer` and `roles`/`creator_role` are mutually exclusive: when `authorizer` is set, the PDP decides every permission-gated route, node list filter (`list_permission`), and identity `/access/v1/*` evaluation across all kinds, and individual routes/verbs may add a `resource: { type, id }` template for record-level checks. Plat5 still enforces authentication, tree membership, org security policies, suspension, credential scope narrowing, rate limits, and audit.

- **Default-deny below the root.** Joining a root grants the role stated on the add or the invite. With a role that holds no permissions for child kinds, members see no projects until a role is bound at a project, or at the org for all of them. A deployment that wants default-allow gives new members a role with those permissions.

- **Denied or missing.** A node you can't use and a node that doesn't exist get the same **403** "permission denied, or it may not exist", so the answer never says whether a node exists. There's no separate visibility rule.

- **Node lists.** Plat5 filters node lists to what the caller can see, so `GET /orgs/{organization}/projects` returns only the projects the caller holds `project`'s `list_permission` on ([lists](#lists)). Services never filter by caller.

- **Outside a request.** The same rule, for a member the service names: work that runs outside a request, and checks about a member other than the caller, ask identity ([outside a request](#outside-a-request)). A service never rebuilds the rule from bindings.


## Who can be in a tree

- **People.** A person's credential (IdP token or personal API key) reaches every tree they're in unless narrowed (by key `allowed_roots` / `scopes` or JWT `AUTH_SCOPE_CLAIM`) or restricted by a root's security policy; the gateway picks the member for the tree in the path or root host, per request. A person also has user routes, for their own data, where they're a user, not a member.

- **Service accounts.** Belong to a node of an org's tree and are deleted with it. They can hold bindings anywhere in their own tree, never outside it. They can't call user routes.

- **Joining.** A person exists from first sign-in, or from an admin adding them. A root is joined by invite or by an admin adding someone. Nodes below a root bind people already in the tree, or guests who redeem an invite below the root.

- **Guests.** A person's member with no effective binding at its root (directly or through a group), only on nodes below it, made by redeeming an invite below the root. Service accounts are never guests. Invites below a root need `guests:write`, checked like any permission: on the invite's node or an ancestor. A deployment turns guests on by granting it, and keeps guest invites in the root's hands by not granting it, or `*`, through any role bindable below the root. Guests are listed on the root's members list, marked, so an admin sees everyone who can reach the tree.

- **Groups.** A flat group of active members (people or service accounts) belonging to one root tree. Groups never nest and never cross trees. In built-in RBAC mode, a group can hold bindings on any node of its tree, and every member of the group inherits those bindings.

- **Closed world.** A tree's data refers only to its own nodes and members. Plat5 guarantees half of this by never sending a principal id, and sending a user id only on user routes and in user events, which carry no tree; the other half is a convention services follow. People cross trees; data doesn't. This is a data-isolation convention, not a privacy boundary: services and the team can still correlate a person across trees, and an operator plane can show which trees a user is in.


## Roles and bindings

| Piece | Says | Owner |
|-------|------|-------|
| Route `permissions` | Which permissions the route needs (any one of them, as a string or array), or `any_member` | The service's file |
| Top-level `authorizer` & route `resource` | Optional external AuthZEN 1.0 PDP (replacing built-in RBAC) and per-route record-level `resource: { type, id }` template | `plat5.yml` / the service's file |
| Role | Which permissions it grants, roles it includes, and where it can be bound | The deployment's roles document, or a root's custom roles (`custom_roles: true`) |
| Binding | This member or group holds this role on this node (optionally with `expires_at`) | Identity, written through the node API |

The gateway checks the caller's permissions on the route's node against the route (from admit in built-in RBAC mode, or from the configured external `authorizer`). It never sees roles or bindings, only permissions, from admit. Services see none of it.

To delegate all authorization to an external policy engine while keeping Plat5's edge enforcement, node tree, membership, and `/access/routes`, configure top-level `authorizer` instead of `roles` ([`reference/config/authorizer.md`](../reference/config/authorizer.md)). Alternatively, in built-in RBAC mode, a deployment can grant `["*"]` on its roles if it wants services to decide access themselves.

A credential may optionally be narrowed below its principal: API keys support permission `scopes`, `allowed_roots` (for personal keys), IP allowlists (`allowed_cidrs`), and `expires_at`; JWTs support optional permission scopes via `AUTH_SCOPE_CLAIM`. Effective permissions are the intersection of the member's resolved permissions and the credential's scopes.

A root may also define an **org security policy** (`GET/PATCH /nodes/{root_id}/security-policy`) with `allow_personal_keys` (disable personal API keys in that tree), `allowed_cidrs` (root-wide IP allowlist enforced by the gateway), and `required_idp_claims` (JWT claim requirements checked against claims listed in top-level `policy_claims` in config; keys outside `policy_claims` are rejected with **422**). Because `required_idp_claims` evaluates claims on JWTs only, pair it with `allow_personal_keys: false` if personal API keys must not bypass IdP requirements such as MFA.

When `custom_roles: true` is enabled in config, each root can create its own ULID-keyed custom roles via identity's API alongside predefined roles; custom roles draw from the deployment's permission catalog, can never grant `*`, and must have a case-insensitive unique `display_name` per root (not colliding with any custom role or system role slug). Setting `custom_roles: false` immediately stops existing custom roles from granting permissions and hides them from role reads.

Bindings may carry an optional `expires_at` (set via `expires_at` or `expires_in`). Expired bindings grant no permissions and are excluded from admit, evaluation, search, and lists; re-binding the same `(subject, node, role)` replaces the expired row, and the member row remains so re-binding keeps the same `member_id`.

The roles document's fields: [`reference/config/roles.md`](../reference/config/roles.md). The permissions identity's and audit's routes require by default: [`reference/config/generated-routes.md`](../reference/config/generated-routes.md#the-generated-permissions).

### Resolution

In built-in RBAC mode, identity resolves permissions at admit, for one member on one node chain ([`reference/api/internal.md`](../reference/api/internal.md#admit)).

1. Take the member's unexpired direct bindings and the unexpired bindings of every group the member belongs to, on every node in the chain: the route's node and each ancestor.
2. Drop any binding whose role isn't in the roles document (or, when `custom_roles: true`, the root's custom roles), or isn't bindable at its node's kind.
3. `permissions` is the union of the remaining roles' permissions, each predefined role's `includes` resolved.

`*` is every permission: the gateway matches it to any route permission. A route can't require `*` itself. `GET …/membership` shows a caller the same answer for themselves.

### Assigning

| Write | Built-in RBAC mode | External `authorizer` mode |
|-------|--------------------|----------------------------|
| Create a node | If `bind_creator` is on, creator gets `creator_role` on it | Creates the node; no role binding is created |
| Create a root | If `bind_creator` is on, creator gets a member + `creator_role` on the root | If `bind_creator` is on, creator gets a member in the new tree (no role) |
| Add a member to a root | `role` in the body, required | Creates the member; `role` must be omitted (**422** if passed) |
| Redeem an invite at a root | The invite's `role` at the root, plus each of its `grants` | Creates the member; `role` and `grants` must be omitted (**422** if passed) |
| Redeem an invite below a root | The invite's `role` at that node (makes a non-member a guest) | Creates the member if not in the tree; `role` must be omitted (**422** if passed) |
| Create a service account | `role` in the body, required. Bound at the node it belongs to | Creates the service account + member; `role` must be omitted (**422** if passed) |
| Bindings, groups, custom roles | `role` required on bindings (`member_id` or `group_id`); optional `expires_at` / `expires_in` | Writes rejected with **422** (policy lives in the external PDP) |

In built-in RBAC mode, a role not in the roles document or the root's custom roles, or not bindable at the node's kind, is **422**. A direct binding for someone who isn't in the tree, or for a group of another tree, is **422**: guests come through invites.

A member or group may hold multiple distinct roles on the same node; `(member_id, node_id, role)` and `(group_id, node_id, role)` are unique among live bindings (creating a binding when an expired one exists for the same tuple replaces it). Changing a binding's role or expiry is a `PATCH` of that binding.

### Who may assign

There is no grant cap. Identity doesn't compare the caller to the role it assigns, or to the member it acts on. Whoever may call a binding route may bind any role bindable there, including one with more permissions than its own. The route's permissions decide who may call; that's the gateway's check.

Service accounts, their keys, and groups are managed under the same permission as bindings, `<kind>:bindings:write` by default: creating a service account binds a role, and adding a member to a group grants them every role bound to that group. A service account is the tree's, not its creator's. Its bindings are its own, and its keys carry them.

So the permissions that reach binding routes are as strong as the strongest role. Give `<kind>:bindings:write` only to members you'd trust with every role bindable at that kind.

## Credentials

Every credential is `Authorization: Bearer`. A value of three dot-separated segments is a JWT; anything else is sent to identity as a key, so identity alone owns the key format ([secrets](../reference/api/identity.md#secrets)).

Any credential works on any route in a kind scope unless restricted: a person's JWT or personal key reaches every tree they're in (subject to key `allowed_roots` and each root's security policy), and admit picks the member for the tree in the path or root host. A service account key works only in its own tree, and never on user routes. A personal key narrowed by `scopes` or `allowed_roots` also cannot call user routes (**403**).

**JWT.** A signed JWT access token from the configured issuer, verified against its JWKS ([`reference/idp.md`](../reference/idp.md)). The gateway reads the issuer, the subject claim (`AUTH_SUBJECT_CLAIM`), the first non-empty name claim of `AUTH_NAME_CLAIMS`, optional permission scopes (`AUTH_SCOPE_CLAIM`), and any policy claims listed in top-level `policy_claims` in config, and sends them to admit as `idp`.

**Key.** Sent to admit as `credential`, as presented. A key may carry an IP allowlist (`allowed_cidrs`), permission `scopes`, `allowed_roots` (personal keys only), and `expires_at` ([`reference/api/identity.md`](../reference/api/identity.md#key)); an expired or revoked key is `valid: false` (**401**), and a client IP outside `allowed_cidrs` is refused with **401**, counted by the failed-auth IP limiter.

## How a request is decided

Every route, in order:

1. **Match.** Unmatched → **404**. Take the chain params from the path (and, when the root kind sets `host`, resolve the root from the matching subdomain), root first (`{organization}`, `{project}`).
2. **Authenticate and admit.** Verify the JWT, or take the key, then admit from cache when possible ([admit](../reference/api/internal.md#what-the-gateway-answers)). A missing or locally refused JWT → **401**. `valid: false` (the credential itself is bad) → **401**. On a user route, a service account or narrowed personal key → **403**. On a kind route, if the root's `allowed_cidrs` excludes the client IP, or admit finds no active member (the node doesn't exist, the caller isn't in the tree, or the root's security policy rejected the credential) → **403** "permission denied, or it may not exist".
3. **Permissions / authorizer.** On a kind route, `any_member` passes once admitted. Otherwise, narrow by credential scopes (if any), and check either built-in RBAC permissions (`*` matches any permission) or the configured top-level `authorizer` (using the route's `resource` template when set, or the route's target node). Denied → **403** "permission denied, or it may not exist", with `permissions` in `details`. Authorizer error/timeout → **503**.
4. **Rate limit.** When rate limiting is enabled and the route has policies, every policy on the route is checked at once (using the root's `rate_limit_tier` override when present) and counted only if all pass ([rate limits](../reference/config/rate-limits.md)). Otherwise **429**.
5. **Fill.** Substitute the placeholders in `upstream`. A param value of `.` or `..`, or one holding `/`, `?` or `#`, is **400**.
6. **Audit intent**, on an audited route with audit on (`sync` mode waits on the write → **503** on failure; `async` mode forwards and queues one completed event after the response; [`concepts/audit.md`](audit.md#delivery)). The gateway's own **403**s, **400**s and **500**s after admit are recorded as one background event.
7. **Forward** with the caller's headers ([`reference/headers.md`](../reference/headers.md)). Slugs never reach a service.
8. **Outcome**, in the background, then respond.

Both **403**s on a kind route, admit finding no member and no permission held, take one path: the same body, `details.permissions` included, both counted by the failed-auth IP limiter (when rate limiting is enabled), and over its budget the same **429**, with only `Retry-After`. So no answer says whether a node exists, not even once the limiter's bucket is full. An `any_member` route names no permissions: its only **403** is the admit one, the caller not in the tree. Both come before the rate limit, so only callers the route lets through spend its budgets: a member with no permission on a project can't drain its `count_at: project` budget. Probing permissions is bounded like probing nodes, by the failed-auth IP limiter. The intent is written after the permission check, so during an audit outage a **503** never says whether a node exists.

**The failed-auth IP limiter** (active when rate limiting is enabled) counts, per [client IP](../reference/gateway.md#client-ip), the **401** for `valid: false` and both **403**s on a kind route. Nothing else is counted: not a JWT refused locally, an unmatched route, a service account's **403** on a user route, or a route check's `false` (a guest loading a page isn't failing auth). Over its budget, a counted answer is **429** instead ([numbers and headers](../reference/gateway.md#rate-limits)).

On a `public` route, authenticate and admit is skipped and there is no caller. On a user route, the chain is empty and the caller is the person's user id.

## How fast changes reach the edge

The gateway caches each admit ([`reference/gateway.md`](../reference/gateway.md#cache)) and long-polls identity's invalidation feed (`GET /internal/admit-invalidations`). A removed binding, group change, custom role or security policy edit, role config apply, deleted/restored/purged node, suspended or deleted person, deleted service account, revoked key, or key IP allowlist change reaches every gateway replica within about a second (milliseconds on a single identity replica). `ADMIT_CACHE_TTL` bounds staleness if the invalidation feed is disconnected. A refusal is cached only for `ADMIT_NEGATIVE_CACHE_TTL`, so a fresh invitee or a new node works within seconds.

## Outside a request

The gateway decides for the caller of a request. Two kinds of service code need the answer for someone else, or later:

- work outside a request: notifications, digests, scheduled reports, outbound webhooks, queued jobs;
- checks about another member inside a request: an assignee, a mention, a share;
- cross-node queries inside a broader scope (for example, listing a member's tasks or searching across every project they can read in an org).

A member can lose a node while staying in its tree (a binding or group membership removed, a role edited), and no event says so. So the service asks identity, when the work runs, whether member M holds permission P on node N ([evaluation](../reference/api/internal.md#access-evaluation)), or which live nodes of kind K under a scope node member M holds permission P on ([resource search](../reference/api/internal.md#resource-search)). Identity answers yes only when the gateway would admit M to a route requiring P on N:

- the node exists, is of the kind asked about, and it and its ancestors are live;
- M is an active, unsuspended member of the node's tree, and admit would accept its principal: a person who isn't suspended, or a service account whose node is live;
- a binding of M's on the node or an ancestor grants P ([resolution](#resolution)), or (when top-level `authorizer` is configured) the external AuthZEN PDP allows `(member:M, P, kind:N)`.

Anything else is no, never an error: a deleted, purged or unknown node, a removed, suspended or unknown member, a member of another tree, a kind that isn't the node's. Identity keeps no cache for these answers, so a removed binding is no at once (an unavailable external authorizer answers **503**).

Checking is the service's job. Plat5 can't see what a service sends or to whom, so a service that skips the check gets no error, only a leak.

## Lists

`/nodes/{node_id}/children/{kind}` and `/users/{user_id}/nodes` return only the nodes the caller can see.

- **Which permission.** The kind's `list_permission`, from config.
- **Where.** At each listed node, from the union of the caller's unexpired bindings on it and its ancestors ([resolution](#resolution)), or from the external `authorizer` when configured.
- **Who.** On a child list, the member in `X-Plat5-Member-Id`, which the gateway sends; without the header the list is the full list, so any front door can list. On `/users/{user_id}/nodes`, that user's active, unsuspended members in every tree.
- **Deleted.** `?status=deleted` uses the same permission, counting the deleted node's own bindings too: they're kept until purge. That includes a deleted org, which has nothing above it: `GET /user/nodes?kind=organization&status=deleted` is how a person finds one. As at admit, only the deleted node's own bindings count beside live ones: a node whose ancestor is deleted too isn't listed, since it can't be restored before that ancestor. Restoring still needs the restore route's permission.

This is why child list routes require `any_member`: anyone in the tree may ask, and the answer is what they can see. A project-only member gets their projects from `GET /orgs/{organization}/projects`, not a 403.

## What a UI shows

A UI hides what the caller can't use, and shows who the caller is on a node. Two answers, both for the caller only:

- **Route checks**, for hiding. The UI sends `POST /access/routes` to the gateway with a batch of requests it might make, by method and public path, and the gateway answers whether each would be allowed: the route match, admit, and permission check or batched external authorizer call, the same steps as a request ([route checks](../reference/gateway.md#route-checks)). The UI asks by route, never by permission name: permissions are the deployment's, and roles can change them with an apply. A node the caller can't reach answers `false`, so a check tells no more than a **403** would.
- **`GET …/membership`**, for who the caller is: their `member_id` and `status` on the node, plus (in built-in RBAC mode) the `bindings` that count there and the `permissions` they grant. A UI shows "Admin" or "Guest" from it, and marks "you".

Both are advisory. The gateway decides every request when it's made, so a page shown on a stale answer gets the usual **403**, and a check can't grant anything.

## Other front doors

Services do no auth, so anything on the private network that speaks a service's API can call it: an operator plane, an internal job, a custom gateway. Each is its own product with its own docs and rules. Base Plat5 defines no operator headers, roles or routes.

What a service can rely on is its own API: paths with node ids, and the headers it documents as required. A front door that calls a service needing `X-Plat5-Member-Id` supplies one, or the service refuses the request as malformed.
