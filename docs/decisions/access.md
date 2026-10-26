# Access

Why access is the union of bindings on a node and its ancestors, decided once at the gateway, and why services see member ids. The rules: [`concepts/access.md`](../concepts/access.md), [`reference/gateway.md`](../reference/gateway.md), [`reference/api/internal.md`](../reference/api/internal.md#admit), [`reference/headers.md`](../reference/headers.md).

## The ancestor rule

| Decision | Why |
|----------|-----|
| A request is allowed when the caller holds the route's permission through a binding on the route's node or an ancestor. Access is the union; there are no deny rules | A binding reaches its node and everything below it, so what a member can do at a node is everything granted on its chain. With no deny rules, adding a binding never takes access away, and the node's access list says who can reach it |
| Roles grant `permissions`; routes have `required_permissions` | A role grants and a route requires: two sides of one check, each named for what it does. The same strings appear on both, so a role permission no route requires is caught at apply |
| A member has many bindings, one role each, on any node of its tree. A project role can exceed the org role | A role at the org reaches every project below it; a role at one project reaches only that project. The union is what a person can do |
| Default-deny below the root is the role a deployment gives new members: one with no project permissions sees no projects until a role is bound at a project, or at the org | Whether a new member sees every project or none is the deployment's choice, made once in the role it gives them, and a binding at one project adds to it. A separate default-deny setting would be a second way to say the same thing |
| Roles are always on, and a role names the kinds it's bindable at | A tree where every member holds every permission isn't a tree. `bindable_at` keeps an org-admin role off a project, where its org permissions would read as if they meant something |
| Removing a role, or a kind from its `bindable_at`, is safe: bindings keep the role's name, grant nothing, and grant again if a role with that name returns | Bindings are the tree's data, and the roles document is the deployment's. A removed role's name shouldn't be reused for something else, since its old bindings would grant the new role |
| Bindings don't expire. Support access is a binding the customer adds and removes | One kind of binding, nothing that ends by itself, and no cap to check on every grant. An optional end time can be added later without breaking anything |
| Access is decided only at the gateway; services enforce only what holds whoever asks | Every service would otherwise reimplement the tree, and each copy can drift |

## Credentials and principals

| Decision | Why |
|----------|-----|
| A person's credential (IdP token or personal key) reaches every tree they're bound in; the gateway picks the binding per request | The bindings already say which trees a person is in. Per-org sessions add a step and grant nothing a binding doesn't |
| Every credential is `Authorization: Bearer`. Three dot-separated segments is a JWT; anything else is a key for identity | One header for every client, and the value's shape tells a JWT from a key, so there's no second header or scheme. Identity alone owns the key format, so the gateway changes with no key change |
| A personal key acts as the person everywhere; keys carry no permissions. A narrower machine is a service account with fewer bindings | A credential narrower than its principal is a second permission system to keep in sync |
| A service account belongs to a node and is deleted with it. It can hold bindings anywhere in its own org's tree, never outside, and can't call user routes | A machine belongs to the part of the tree that made it, and goes when that part does. It can work across its org, but the org is the isolation boundary. User routes are a person's own |
| A project-scoped key is a service account bound only to that project | A key is exactly as wide as its service account's bindings, and the gateway's check is the ancestor rule: no outside call and no new failure mode |
| Suspension is per person. A member is removed instead | Suspending someone in one org is removing them there |
| A person's name comes only from the IdP's name claim, `users.name_claim`. An empty claim leaves the name as it is | The IdP is the source of truth for who someone is. An IdP that sends `""` hasn't told us a name; blanking it on every request would be worse |
| A person has a user id, sent only on user routes, and one member per tree. Their principal id never leaves identity | Per-person data (a profile, a setting) needs a key; tree data is keyed by members, so the user id can't join two trees' data |
| People are users in identity's service API (`/users/{user_id}`, its status, account deletion, `GET /members/{member_id}/user`, and an exact lookup by IdP issuer and subject), never in a default public route | An operator plane must reach a person's status and account from an org member id. Services still see user ids only on user routes |

## Assigning access

| Decision | Why |
|----------|-----|
| People join a root by invite or admin add, and a node below a root as a guest by invite | Everyone with access appears on the root's members list, as a member or a guest |
| A guest is a person's member with no binding on the root. A service account is never a guest | Someone brought into one project without joining the org shows as exactly that: in the tree, with no reach at the root. A service account that belongs below the root is the tree's own |
| Inviting guests takes `guests:write`, with no kind in the name, checked like any permission on the invite's node or an ancestor. A deployment that wants guest invites only from the root grants it, and `*`, through no role bindable below the root | It isn't about a level: a binding's reach is where it sits and below. Who may bring in outsiders is the deployment's policy, so it's a permission its roles grant or withhold, not a setting |
| Invite tokens are hashed at rest and shown once, on create, like keys. Lists show `token_prefix`, and listing isn't audited | A token that can be read back is a stored secret, and anyone who could read the list could redeem it. The prefix is enough to tell invites apart |
| `role` is required on every invite. `max_uses` omitted or `null` is 1; unlimited is `unlimited`. `expires_in` is a duration, default `7d`, at most `30d` | `null` meaning unlimited is the unsafe default for typed clients. One duration form across Plat5 |
| A redeem by someone already bound on the invite's node changes nothing and isn't a use. Redeem answers the member and the node, with its ancestors | "No change" means none. The console needs the member and where to go next, and builds its own URL from the chain |
| An invite's grants name live nodes below its root, each once. At redeem, grants on nodes no longer live are skipped | A grant on a deleted node can't take effect; the rest of the invite still can |
| Members are added by IdP subject with a required `role`, removed, leave (`DELETE …/membership` on the root), and are looked up by id with `any_member` | Consoles must show "Dana (former member)" to anyone who sees her records |
| The creator of a node always gets `creator_role` on it. A node created with no member behind the request gets no binding | Whoever makes a node can run it from the start, whatever their role on the parent. Skipping the binding when an inherited one holds the same role name would compare names, not permissions |
| Service accounts and their keys are created under `<kind>:bindings:write`, with no permission of their own | Creating a service account binds any role, so a separate permission would be as strong as `bindings:write` anyway |
| Identity reads the caller on three routes only: creating a child and child lists read `X-Plat5-Member-Id`, creating a root reads `X-Plat5-User-Id`. A header naming no active member or user is **400** | Every other route takes ids in the path, so any front door can call it. The service refuses a malformed request |
| A repeat delete is **204** everywhere: members, bindings, service accounts, keys, invites | Repeat deletes succeed across the API |
| Writes that touch members take the tree lock, like structural writes | One active member per principal per tree, and "removed when left with no bindings", can't race |
| There's no last-owner guard. An org left without an admin is recovered through an operator plane | The config decides who may remove whom; a guard would second-guess it and block account deletion |

## How a request is decided

| Decision | Why |
|----------|-----|
| Identity's admit answers for one credential on one chain: the member, the nodes and the permissions for that request. An empty chain is a user route. The gateway caches the answer | Every fact it needs is in Plat5, so the check makes no outside call |
| Every admit answer has every field. A missing node and a caller outside the tree both answer `member_id: null` with empty `nodes` | The gateway reads a missing field as identity failing (**503**). The two cases take the same branch, so one answer is enough |
| Admit resolves `chain` by kind name and never checks it against identity's kinds. Only an unreadable body is **422** | The gateway and identity read the config on their own schedules. A request in that second must fail like any missing node (**403**), not as an outage |
| `permissions` is sorted, with `*` as the role wrote it; the gateway matches `*` | One request gives one answer, which keeps tests and logs stable |
| Denied and missing are both **403** "permission denied, or it may not exist" | Any other answer would tell an outsider which nodes exist, and there's no separate visibility rule to keep consistent |
| Not found and not in the tree are **403** before the rate limit; a missing permission is **403** after it. Same body | A caller outside a tree can't spend its budget, and existence doesn't leak |
| The failed-auth IP limiter counts only what identity refused: an invalid key, a node not found, a caller outside the tree. A JWT refused locally and an unmatched route don't count | Those cost no identity call and no Valkey trip, and counting them throttles single-page apps behind one NAT |
| The admit cache keeps a refusal only briefly (`ADMIT_NEGATIVE_CACHE_TTL`, about `10s`), and keys a verified JWT by issuer and subject | A fresh invitee or a new node works within seconds, and a refreshed token is the same entry |
| The admit cache key includes `allow_deleted` | Otherwise a refusal cached by a read of a deleted node answers the restore route too |
| On the restore route (`allow_deleted`), a deleted node's own bindings, kept until purge, still count | A delete revokes nothing, so the people who managed the node can still undo it. A deleted root has nothing above it to grant the restore |
| `GET …/membership` returns the caller's member id, the bindings that count at the node, and the permissions they grant | A console shows "Admin" or "Guest", marks "you", and hides buttons the caller can't use, without guessing from role names |

## At the edge

| Decision | Why |
|----------|-----|
| The accepted JWT audiences are required; `*` accepts any, explicitly | A gateway that skips the audience check by omission accepts tokens minted for other apps on the same IdP |
| `AUTH_SUBJECT_CLAIM` defaults to `sub` | OIDC's subject claim |
| Env vars take base URLs; the admit path is fixed in code | One form for every URL, and the path is identity's API, not wiring |
| A param value of `.` or `..` is **400** | It would change the path the upstream sees |
| `max_body_bytes` per service, with a deployment default | An upload service and a JSON API need different limits |
| The gateway adds a security header only when the upstream response hasn't set it | A service that sets its own `X-Frame-Options` for an embeddable page means it |
| CORS allows `*` only when written as `allowed_origins: ["*"]`; an empty list or no block allows no cross-origin request | An empty list meaning "everything" is the unsafe reading of nothing. Explicit config over inference |
| `Allow-Headers` adds `traceparent`, `tracestate` and the config's `allowed_headers` to a base list; `Expose-Headers` lists `X-Request-ID`, `Retry-After` and `X-RateLimit-*` | Browsers need to send trace context and read the request id and rate-limit state |

## Rate limits

| Decision | Why |
|----------|-----|
| Policies live in `plat5.yml` and are shared by name: every route naming a policy shares its bucket | One model instead of five, and changing a limit is an apply, not a restart |
| Every config has a `default` policy, without `count_at`, and it's every route's that names none. An unlimited route says `rate_limit: false` | No route is unlimited by omission. `default` reaches user and public routes, which have no node to count at |
| A policy with no `count_at` counts each caller at the route's node; `count_at` names the kind whose node everyone shares | Each caller has their own budget at each node, so one busy script can't throttle a whole org. A tenant-wide cap is the explicit choice |
| All of a route's policies are checked at once and counted only if all pass, in one Valkey round trip | A request one bucket refuses spends nothing from the others |
| The headers show the policy with the fewest requests remaining; a **429** carries the longest `Retry-After` among those exceeded | The longest wait is the one after which every policy passes again |
| Counter keys carry a fixed Plat5 prefix | A Valkey shared with other software is safe |
| Monthly and per-plan counts stay in services | They're product rules with product data, not request-rate protection |

## Lists

| Decision | Why |
|----------|-----|
| Each kind names its `list_permission` in `plat5.yml`, required; child lists require `any_member` and filter by it | Said directly, a missing one is an apply error, not a silently empty list. A project-only member has no `project:read` at the org, so a permission on the list route would hide projects they can open. Filtering by the kind's own permission lists exactly the nodes the caller can open |
| `list_permission` also filters `?status=deleted`, counting the deleted node's own bindings | A deleted node's bindings are kept until purge, so whoever could see it can find it to restore. Restoring still needs the restore route's permission |
| A deleted node is listed only when every node above it is live | Admit counts a deleted node's own bindings only as the last of a live chain, and such a node can't be restored before its ancestor anyway |
| Child lists filter by `X-Plat5-Member-Id` when it's sent and return the full list when it isn't | Any front door can list |
| One access list: `GET …/bindings` lists every binding that counts at the node, each with where it sits, whether it's inherited, and whether it's a guest's. `?inherited=false` for the node's own | A member can hold several bindings that count, each with its own role. Two overlapping lists drift apart |
| `?status=` everywhere, matching the node field | One name for one thing |
| Nodes carry `ancestors`, root first, and `purge_after` | A console shows "Gruow / gatheroom" and "restorable until" without a second call or recomputing the grace. Plat5 returns the chain, not a URL: the console owns its URL scheme |
| Response shapes are in the identity API; key lists include revoked keys | One place for each shape |

## What services see

| Decision | Why |
|----------|-----|
| Every tree route the gateway admits carries the node ids, the caller's member id (`X-Plat5-Member-Id`) and `X-Plat5-Principal`. Never a principal id | Authorship needs the caller, and operators need to act as someone. A principal id is the same in every tree, so passing it would let two trees' data be joined on one person |
| The member id is per tree; the user id is sent only on user routes, as `X-Plat5-User-Id` | Two orgs can't join their data on a shared person ([boundaries](boundaries.md#each-tree-is-a-closed-world)) |
| Records store `X-Plat5-Member-Id` as their author when logic needs it ("published by", "you can't approve your own change") | Separation of duties is a service comparing two member ids on two records: a state rule, not an access check |
| Removed is final: re-adding someone makes a new member id; the old member stays, and its lookup returns its name and `status: removed` | A changelog can show "Dana (former member)", and two stints in an org stay distinct |
| A member stores its principal kind (`members.principal`) | The member shape names `person` or `service_account`, and a removed service account or deleted account has no principal row left |
| Services do no auth, so any front door can call them | The operator plane and internal jobs call the same service API under their own documented rules |
