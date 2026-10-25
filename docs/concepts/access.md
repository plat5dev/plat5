# Access

Who can do what, in one place: the ancestor rule, who can be in a tree, roles and bindings, credentials, how the gateway decides a request, and what lists show. The shapes these rules act on are in [reference](../README.md#reference).

## The rule

- **Ancestor rule.** A request is allowed when the caller holds one of the route's permissions through a binding on the route's node or an ancestor. Access is the union of those bindings. There are no deny rules.

- **Default-deny below the root.** Joining a root grants the role stated on the add or the invite. With a role that holds no permissions for child kinds, members see no projects until a role is bound at a project, or at the org for all of them. A deployment that wants default-allow gives new members a role with those permissions.

- **Denied or missing.** A node you can't use and a node that doesn't exist get the same **403** "permission denied, or it may not exist", as Google Cloud answers. There's no separate visibility rule.

- **Node lists.** Plat5 filters node lists to what the caller can see, so `GET /orgs/{organization}/projects` returns only the projects the caller holds `project`'s `list_permission` on ([lists](#lists)). Services never filter by caller.


## Who can be in a tree

- **People.** A person's credential (IdP token or personal API key) reaches every tree they're in; the gateway picks the member for the tree in the path, per request. A personal key acts as the person everywhere. Anything narrower is a service account. A person also has user routes, for their own data, where they're a user, not a member.

- **Service accounts.** Born at a node of an org's tree and deleted with it. They can hold bindings anywhere in their own tree, never outside it. They can't call user routes.

- **Joining.** A person exists from first sign-in, or from an admin adding them. A root is joined by invite or by an admin adding someone. Nodes below a root bind people already in the tree, or guests who redeem an invite below the root.

- **Guests.** A person's member with no binding at its root, only on nodes below it, made by redeeming an invite below the root. Service accounts are never guests. Invites below a root need `guests:write`, checked like any permission: on the invite's node or an ancestor. A deployment turns guests on by granting it, and keeps guest invites in the root's hands by not granting it, or `*`, through any role bindable below the root. Guests are listed on the root's members list, marked, as GitHub lists outside collaborators.

- **Closed world.** A tree's data refers only to its own nodes and members. Plat5 guarantees half of this by never sending a principal id, and sending a user id only on user routes; the other half is a convention services follow. People cross trees; data doesn't.


## Roles and bindings

| Piece | Says | Owner |
|-------|------|-------|
| Route `required_permissions` | Which permissions the route needs (any one of them), or `any_member` | The service's file |
| Role | Which permissions it grants, roles it includes, and where it can be bound | The deployment's roles document |
| Binding | This member holds this role on this node | Identity, written through the node API |

The gateway checks the caller's permissions on the route's node against the route. It never sees roles or bindings, only permissions, from admit. Services see none of it.

Roles are always on. There's no deployment-wide switch: a deployment where something else decides access gives its roles `["*"]`.

A credential carries its principal's bindings in the tree it's used in. Keys have no permissions of their own. To give a machine less, give it a service account with fewer or narrower bindings.

Bindings don't expire. Time-boxed access, such as support for a week, is a binding someone adds and removes.

The roles document's fields: [`reference/config/roles.md`](../reference/config/roles.md). The permissions identity's and audit's routes require by default: [`reference/config/resources.md`](../reference/config/resources.md#the-default-permissions).

### Resolution

Identity resolves permissions at admit, for one member on one node chain ([`reference/api/internal.md`](../reference/api/internal.md#admit)).

1. Take the member's bindings on every node in the chain: the route's node and each ancestor.
2. Drop any binding whose role isn't in the document, or isn't bindable at its node's kind.
3. `permissions` is the union of the remaining roles' permissions, each role's `includes` resolved.

`*` is every permission: the gateway matches it to any route permission. A route can't require `*` itself. `GET …/membership` shows a caller the same answer for themselves.

### Assigning

| Write | Role |
|-------|------|
| Create a node | The creator gets `creator_role` on it, always, as Google Cloud binds a project's creator. A node created with no member behind the request gets no binding |
| Create a root | The user creating it gets a member in the new tree, with `creator_role` on the root |
| Add a member to a root | `role` in the body, required |
| Redeem an invite at a root | The invite's `role` at the root, plus each of its `grants` |
| Redeem an invite below a root | The invite's `role` at that node. For someone not in the tree, that makes them a guest |
| Create a service account | `role` in the body, required. Bound at the node it's born at |
| Create or change a binding | `role` in the body, required |

A role not in the document, or not bindable at the node's kind, is **422**. A direct binding for someone who isn't in the tree is **422**: guests come through invites.

One binding per member per node. Changing a member's role on a node is a `PATCH` of that binding.

### Who may assign

There is no grant cap. Identity doesn't compare the caller to the role it assigns, or to the member it acts on. Whoever may call a binding route may bind any role bindable there, including one with more permissions than its own. The route's permissions decide who may call; that's the gateway's check.

Service accounts are created and their keys made under the same permission as bindings, `<kind>:bindings:write` by default: creating a service account binds a role, so it's a binding write. A service account belongs to its tree, not to whoever created it. Its bindings are its own, and its keys carry them.

So the permissions that reach binding routes are as strong as the strongest role. Give `<kind>:bindings:write` only to members you'd trust with every role bindable at that kind.

## Credentials

Every credential is `Authorization: Bearer`. A value of three dot-separated segments is a JWT; anything else is sent to identity as a key, so identity alone owns the key format ([credentials](../reference/api/identity.md#credentials)), as Stripe tells keys apart by shape.

Any credential works on any route in a kind scope: a person's JWT or personal key reaches every tree they're in, and admit picks the member for the tree in the path. A service account key works only in its own tree, and never on user routes.

**JWT.** Verified against JWKS ([`reference/idp.md`](../reference/idp.md)). The gateway reads the issuer, the subject claim (`AUTH_SUBJECT_CLAIM`) and the name claim (`users.name_claim`), and sends them to admit as `idp`.

**Key.** Sent to admit as `credential`, as presented.

## How a request is decided

Every route, in order:

1. **Match.** Unmatched → **404**. Take the chain params from the path, root first (`{organization}`, `{project}`).
2. **Authenticate and admit.** Verify the JWT, or take the key; then admit, from cache when possible ([admit](../reference/api/internal.md#what-the-gateway-answers)). A JWT refused locally → **401**. `valid: false` → **401**. On a kind route, `member_id: null` (the node doesn't exist, or the caller isn't in its tree) → **403** "permission denied, or it may not exist". Only identity's refusals count against the failed-auth IP limiter.
3. **Rate limit.** Every policy on the route is checked at once, and counted only if all pass ([rate limits](../reference/config/rate-limits.md)). Otherwise **429**.
4. **Permissions.** On a kind route, the caller's permissions share none with the route's (and the route isn't `any_member`) → **403** "permission denied, or it may not exist", with `required_permissions` in `details`. `*` matches any permission.
5. **Fill.** Substitute the placeholders in `upstream`. A param value of `.` or `..`, or one holding `/`, `?` or `#`, is **400**.
6. **Audit intent**, on an audited route with audit on → **503** if it can't be written ([`concepts/audit.md`](audit.md#request-order)). The gateway's own **403**s, **400**s and **500**s after admit are recorded as one background event.
7. **Forward** with the caller's headers ([`reference/headers.md`](../reference/headers.md)). Slugs never reach a service.
8. **Outcome**, in the background, then respond.

Both **403**s have the same body, `details.required_permissions` included, so neither says whether the node exists. A node you can't use and a node that doesn't exist answer the same, as Google Cloud's "Permission denied on resource (or it may not exist)". The admit **403** runs before the rate limit so a caller outside a tree can't spend that tree's budget; the permission **403** runs after, so a member probing permissions is bounded by its own limit. The intent is written after the permission check, so during an audit outage a **503** never says whether a node exists.

On a `public` route, authenticate and admit is skipped and there is no caller. On a user route, the chain is empty and the caller is the person's user id.

## How fast changes reach the edge

The gateway caches each admit ([`reference/gateway.md`](../reference/gateway.md#cache)). A removed binding, a role change, a deleted node, a suspended person or a revoked key reaches the edge within `ADMIT_CACHE_TTL`. A refusal is cached only for `ADMIT_NEGATIVE_CACHE_TTL`, so a fresh invitee or a new node works within seconds.

## Lists

`/nodes/{node_id}/children/{kind}` and `/users/{user_id}/nodes` return only the nodes the caller can see, as Google Cloud's project search returns only projects the caller has `resourcemanager.projects.get` on.

- **Which permission.** The kind's `list_permission`, from `plat5.yml`.
- **Where.** At each listed node, from the union of the caller's bindings on it and its ancestors ([resolution](#resolution)).
- **Who.** On a child list, the member in `X-Plat5-Member-Id`, which the gateway sends; without the header the list is the full list, so any front door can list. On `/users/{user_id}/nodes`, that user's members in every tree.
- **Deleted.** `?status=deleted` uses the same permission, counting the deleted node's own bindings too: they're kept until purge, as Google Cloud lists deleted projects to anyone with `projects.get`. That includes a deleted org, which has nothing above it: `GET /user/nodes?kind=organization&status=deleted` is how a person finds one. As at admit, only the deleted node's own bindings count beside live ones: a node whose ancestor is deleted too isn't listed, since it can't be restored before that ancestor. Restoring still needs the restore route's permission.

This is why child list routes require `any_member`: anyone in the tree may ask, and the answer is what they can see. A project-only member gets their projects from `GET /orgs/{organization}/projects`, not a 403.

## Other front doors

Services do no auth, so anything on the private network that speaks a service's API can call it: an operator plane, an internal job, a custom gateway. Each is its own product with its own docs and rules. Base Plat5 defines no operator headers, roles or routes.

What a service can rely on is its own API: paths with node ids, and the headers it documents as required. A front door that calls a service needing `X-Plat5-Member-Id` supplies one, or the service refuses the request as malformed.
