# Access

Who can do what, in one place: the ancestor rule, who can be in a tree, roles and bindings, credentials, how the gateway decides a request, and what lists show. The shapes these rules act on are in [reference](../README.md#reference).

## The rule

- **Ancestor rule.** A request is allowed when the caller holds one of the route's labels through a binding on the route's node or an ancestor. Access is the union of those bindings. There are no deny rules.

- **Default-deny below the root.** Joining a root grants `default_role` there. With a default role that holds no labels for child kinds, members see no projects until a role is bound at a project, or at the org for all of them. A deployment that wants default-allow gives its default role those labels.

- **Denied or missing.** A node you can't use and a node that doesn't exist get the same **403** "permission denied, or it may not exist", as Google Cloud answers. There's no separate visibility rule.

- **Node lists.** Plat5 filters node lists to what the caller can read, so `GET /orgs/{organization}/projects` returns only the projects the caller can read ([lists](#lists)). Services never filter by caller.


## Who can be in a tree

- **People.** A person's credential (IdP token or personal API key) reaches every tree they're in; the gateway picks the member for the tree in the path, per request. A personal key acts as the person everywhere. Anything narrower is a service account.

- **Service accounts.** Born at a node of an org's tree and deleted with it. They can hold bindings anywhere in their own tree, never outside it, and never in the directory.

- **Joining.** People join the directory at first sign-in. A root is joined by invite or by an admin adding someone. Nodes below a root bind people already in the tree, or guests who redeem an invite below the root.

- **Guests.** A member with no binding at its root, only on nodes below it, made by redeeming an invite below the root. Invites below a root need the root's `<root kind>:guests:write`, so a deployment turns guests on by granting that label. Guests are listed on the root's access list as guests, as GitHub lists outside collaborators.

- **Closed world.** A tree's data refers only to its own nodes and members. Plat5 guarantees half of this by never sending a person's principal id; the other half is a convention services follow. People cross trees; data doesn't.


## Roles and bindings

| Piece | Says | Owner |
|-------|------|-------|
| Route `required_labels` | Which labels the route needs (any one of them) | The service's `routes.yml` |
| Role | Which labels it grants, and where it can be bound | The deployment's roles document |
| Binding | This member holds this role on this node, until `expires_at` | Identity, written through the node API |

The gateway checks the caller's labels on the route's node against the route. It never sees roles or bindings, only labels, from admit. Services see none of it.

Roles are always on. There's no deployment-wide switch: a deployment where something else decides access gives its roles `["*"]`.

A credential carries its principal's bindings in the tree it's used in. Keys have no labels of their own. To give a machine less, give it a service account with fewer or narrower bindings.

The roles document's fields: [`reference/config/roles.md`](../reference/config/roles.md). The default labels each kind's generated routes use: [`reference/api/edge-catalog.md`](../reference/api/edge-catalog.md#generated-labels).

### Resolution

Identity resolves labels at admit, for one member on one node chain ([`reference/api/internal.md`](../reference/api/internal.md#admit)).

1. Take the member's bindings on every node in the chain: the route's node and each ancestor.
2. Drop any binding past its `expires_at`, whose role isn't in the document, or whose role isn't bindable at its node's kind.
3. `labels` is the union of the remaining roles' labels.

`*` is every label: the gateway matches it to any route label. A route can't require `*` itself.

Admit also returns `good_until`, the earliest `expires_at` among the bindings that contributed, so the gateway's cache never outlives an expiring grant. Any other change (a role edit, a removed binding) reaches the edge within `APIKEY_CACHE_TTL_SECS`.

### Assigning

| Write | Role |
|-------|------|
| Create a node | The creator gets `creator_role` on it, unless an inherited binding already holds `creator_role`. A node created with no member behind the request gets no binding |
| Add a member to a root | `role` in the body, or `default_role` |
| Redeem an invite at a root | The invite's `role` at the root, plus each of its `grants` |
| Redeem an invite below a root | The invite's `role` at that node. For someone not in the tree, that makes them a guest |
| Create a service account | `role` in the body, required. Bound at the node it's born at |
| Create or change a binding | `role` in the body, required |

A role not in the document, or not bindable at the node's kind, is **422**. An `expires_at` past the role's `max_duration`, or missing when the role has one, is **422**. A direct binding for someone who isn't in the tree is **422**: guests come through invites.

One binding per member per node. Changing a member's role on a node is a `PATCH` of that binding.

### Who may assign

There is no grant cap. Identity doesn't compare the caller to the role it assigns, or to the member it acts on. Whoever may call a binding route may bind any role bindable there, including one with more labels than its own. The route's labels decide who may call; that's the gateway's check.

A service account belongs to its tree, not to whoever created it. Its bindings are its own, and its keys carry them.

So the labels that reach binding routes are as strong as the strongest role. Give `<kind>:bindings:write` only to members you'd trust with every role bindable at that kind.

## Credentials

`Authorization: Bearer` is the IdP JWT only. Plat5 API keys are on `X-API-Key`. If `X-API-Key` is present, it is the credential; don't fall through to the other header.

Any credential works on any route scope: a person's JWT or personal key reaches every tree they're in, and admit picks the member for the tree in the path. A service account key works only in its own tree, and never on sign-in routes.

**JWT.** Verified against JWKS ([`reference/idp.md`](../reference/idp.md)). The gateway reads the issuer, the subject claim (`AUTH_SUBJECT_CLAIM`) and the name claim (the sign-in kind's `name_claim`), and sends them to admit as `idp`.

**`X-API-Key`.** The gateway checks the shape first: `{brand}-k-2-{secret}`, with a brand of `[a-z][a-z0-9]*` up to 32 characters and a 43-character base64url secret. Any other shape is **401** without an identity call, and counts against the failed-auth limiter. The gateway doesn't know the brand.

## How a request is decided

Every route, in order:

1. **Match.** Unmatched → **404**. Take the chain params from the path, root first (`{organization}`, `{project}`).
2. **Authenticate and admit.** Verify the JWT, or check the key's shape; then admit, from cache when possible ([the gateway's admit table](../reference/gateway.md#admit)). `valid: false` → **401**. Node not found, or the caller has no member in the tree → **403** "permission denied, or it may not exist", counted against the failed-auth IP limiter.
3. **Rate limit.** Each policy on the route counts in the bucket of the node at its `count_at` ([`reference/config/routes.md`](../reference/config/routes.md#rate_limit)). Every policy must pass → **429**.
4. **Audit intent**, on an audited route with audit on, with the node chain → **503** if it can't be written ([`concepts/audit.md`](audit.md#request-order)).
5. **Labels.** The caller's labels share none with the route's (and the route isn't `[]`) → **403** "permission denied, or it may not exist", with `required_labels` in `details`. `*` matches any label.
6. **Fill and forward.** Substitute `{subject.<kind>_id}`, `{subject.member_id}` and `{path.*}` in `upstream` (a bad path param → **400**, a bad subject id → **500**), set the subject headers, forward. Slugs never reach a service.
7. **Outcome**, in the background, then respond.

Both **403**s have the same body, `details.required_labels` included, so neither says whether the node exists. A node you can't use and a node that doesn't exist answer the same, as Google Cloud's "Permission denied on resource (or it may not exist)". Step 2's **403** runs before the rate limit so a caller outside a tree can't spend that tree's budget; step 5's runs after, so a member probing labels is bounded by its own tree's limit.

On a `public` route, step 2 is skipped and there is no subject. On a sign-in route (`scope: directory`), the chain is empty and the subject is the person's directory member.

## How fast changes reach the edge

The gateway caches each admit ([`reference/gateway.md`](../reference/gateway.md#cache)).

What the TTL bounds: a removed binding, a role change, a deleted node, a suspended person or a revoked key reaches the edge within the TTL. An expiring binding is exact, through `good_until`. Raw keys and JWTs are hashed before use as cache keys. `APIKEY_CACHE_TTL_SECS` covers every admit; don't rename it.

## Lists

`/nodes/{node_id}/children/{kind}` and `/members/{member_id}/nodes` return only the nodes the caller could read, as Google Cloud's project search returns only projects the caller has `resourcemanager.projects.get` on.

- **Which labels.** The labels of the kind's read route: the applied `GET` route to identity whose `upstream` is `/nodes/{subject.<kind>_id}`. Identity reads them from the services in its control-plane feed. With several such routes, any of their labels counts. With none applied, nothing is readable and the list is empty.
- **Where.** At each listed node, from the union of the caller's bindings on it and its ancestors ([resolution](#resolution)).
- **Deleted.** `?state=deleted` uses the kind's delete route (`DELETE` to `/nodes/{subject.<kind>_id}`) the same way, counting the deleted node's own bindings too: they're kept until purge, so whoever could delete a node can find and restore it. That includes a deleted org, which has nothing above it. `GET /me/nodes?kind=organization&state=deleted` is how a person finds one.

This is why the generated list routes are `[]`: anyone in the tree may ask, and the answer is what they can read. A project-only member gets their projects from `GET /orgs/{organization}/projects`, not a 403.

## Other front doors

Services do no auth, so anything on the private network that speaks a service's API can call it: an operator plane, an internal job, a custom gateway. Each is its own product with its own docs and rules. Base Plat5 defines no operator headers, roles or catalog rules.

What a service can rely on is its own API: paths with node ids, and the headers it documents as required. A front door that calls a service needing `X-Plat5-Member-Id` supplies one, or the service refuses the request as malformed.
