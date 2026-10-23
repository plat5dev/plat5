# Access

Why access is the union of bindings on a node and its ancestors, decided once at the gateway, and why services see member ids. The rules: [`concepts/access.md`](../concepts/access.md), [`reference/gateway.md`](../reference/gateway.md), [`reference/api/internal.md`](../reference/api/internal.md#admit), [`reference/headers.md`](../reference/headers.md).

## The ancestor rule

| Decision | Why |
|----------|-----|
| A request is allowed when the caller holds the route's label through a binding on the route's node or an ancestor. Access is the union; there are no deny rules | Google Cloud's effective policy is the union of inherited policies ([IAM hierarchy](https://docs.cloud.google.com/iam/docs/resource-hierarchy-access-control)). Kubernetes RBAC is additive, and a ClusterRole can be bound in one namespace ([RBAC](https://kubernetes.io/rbac/)) |
| A member has many bindings, one role each, on any node of its tree. A project role can exceed the org role | A role at the org reaches every project below it; a role at one project reaches only that project. The union is what a person can do |
| Default-deny below the root is the deployment's choice, through `default_role`: a `member` role with no project labels sees no projects until a role is bound at a project, or at the org | GitHub base permissions set what every org member gets on every repository, and can be none; a higher grant on one repository wins ([GitHub](https://docs.github.com/en/enterprise-server@3.17/organizations/managing-user-access-to-your-organizations-repositories/managing-repository-roles/setting-base-permissions-for-an-organization)) |
| Roles are always on, and a role names the kinds it's bindable at | A tree where every member holds every label isn't a tree. `bindable_at` keeps an org-admin role off a project, as Kubernetes keeps a Role inside its namespace |
| Removing a role, or a kind from its `bindable_at`, is safe: bindings keep the role's name and grant nothing until it's back | A deleted Google Cloud custom role behaves the same way |
| Bindings can expire, and a role can cap how long (`max_duration`) | Google Cloud expiring access ([temporary access](https://docs.cloud.google.com/iam/docs/configuring-temporary-access)). Support access is a binding the customer adds for 7 days: visible, consented to, and self-ending |
| Access is decided only at the gateway; services enforce only what holds whoever asks | Every service would otherwise reimplement the tree, and each copy can drift |

## Credentials and principals

| Decision | Why |
|----------|-----|
| A person's credential (IdP token or personal key) reaches every tree they're bound in; the gateway picks the binding per request | A cloud console login works in every project the user has a role in. Per-org sessions add a step and grant nothing a binding doesn't |
| A personal key acts as the person everywhere; keys carry no labels. A narrower machine is a service account with fewer bindings | A credential narrower than its principal is a second permission system to keep in sync |
| A service account is born at a node and deleted with it. It can hold bindings anywhere in its own org's tree, never outside, never in the directory | Google Cloud service accounts live in one project and can be used in others ([cross-project](https://docs.cloud.google.com/eventarc/docs/use-cross-project-service-account)); Plat5 stops at the org, the isolation boundary |
| A project-scoped key is a service account bound only to that project | A key is exactly as wide as its service account's bindings, and the gateway's check is the ancestor rule: no outside call and no new failure mode |
| Creating a service account requires `role`, and takes `expires_at`, required when the role has a `max_duration` | Otherwise a role with `max_duration` could never be given to a new service account; member add has the same field |
| Suspension is per person. A member is removed instead | Suspending someone in one org is removing them there |
| A person's name comes only from the IdP's name claim, the sign-in kind's `name_claim`. An empty claim leaves the name as it is | The IdP is the source of truth for who someone is. An IdP that sends `""` hasn't told us a name; blanking it on every request would be worse |
| Adding someone who has never signed in creates the person and their directory member together | Every person has one directory member; there's no state without one |

## Assigning access

| Decision | Why |
|----------|-----|
| People join the sign-in kind on first sign-in, a root by invite or admin add, and a node below a root as a guest by invite | Everyone with access appears on the root's access list, as a member or a guest |
| A guest is a member with no binding on its tree's root. Guest invites below a root need `role` and `organization:guests:write` | GitHub's outside collaborators work this way ([GitHub](https://docs.github.com/en/organizations/managing-user-access-to-your-organizations-repositories/managing-outside-collaborators/adding-outside-collaborators-to-repositories-in-your-organization)) |
| Invite tokens are hashed at rest and shown once, on create, like keys. Lists show `token_prefix`, and listing isn't audited | A token that can be read back is a stored secret. Stripe and GitHub show secrets once |
| Invites store `role_duration`, and each grant its `duration`, in the config's duration form. The role is fixed at create: `default_role` if omitted at a root | One duration form across Plat5. `default_role` is null below a root |
| `expires_at` on member add and `role_duration` on invites; `default_role` and `creator_role` have no `max_duration` | A `max_duration` role is otherwise impossible to grant at join |
| Redeem by someone already bound on the invite's node is a use and changes nothing, grants included. Redeem answers `{ member_id, root_id, node_id }` | "No change" means none. The console needs the member and where to go next |
| An invite's grants name live nodes below its root, each once. At redeem, grants on nodes no longer live are skipped | A grant on a deleted node can't take effect; the rest of the invite still can |
| Members are added by IdP subject, removed, leave (`DELETE /orgs/{organization}/me`), and are looked up by id with `[]` labels | Consoles must show "Dana (former member)" to anyone who sees her records |
| The creator of a node gets `creator_role` unless an inherited binding with no `expires_at` already holds it. A node created with no member behind the request gets no binding | An expiring inherited grant would take the creator's access to their new node with it |
| `X-Plat5-Member-Id` naming no active member of the node's tree is **400**. On `POST /roots/{kind}` it's optional and, when sent, must be an active directory member | The service refuses a malformed request. A front door may create a root with no member behind it, as it may any node |
| `PATCH` of a binding with `expires_at: null` clears it; the result must still meet the role's `max_duration` | Plain, and the cap holds either way |
| Removing a member already removed is **204** and changes nothing | Repeat deletes succeed across the API |
| Writes that touch members take the tree lock, like structural writes | One active member per principal per tree, and "removed when left with no bindings", can't race |
| There's no last-owner guard. An org left without an admin is recovered through the operator plane | The config decides who may remove whom; a guard would second-guess it and block account deletion |

## How a request is decided

| Decision | Why |
|----------|-----|
| Identity's admit answers for one credential on one chain: the member, the nodes and the labels for that request, with a `good_until`. The gateway caches the answer | Every fact it needs is in Plat5, so the check makes no outside call |
| Every admit answer has every field. With `valid: false` they're empty or null; with `found: false`, `nodes` is empty too. On a sign-in route `nodes` is empty | The gateway reads a missing field as identity failing (**503**). A partial chain tells it nothing it acts on |
| Admit resolves `chain` by kind name and never checks it against identity's kinds. Only an unreadable body is **422** | The gateway and identity read the config on their own schedules. A request in that second must fail like any missing node (**403**), not as an outage |
| `labels` is sorted, with `*` as the role wrote it; the gateway matches `*` | One request gives one answer, which keeps tests and logs stable |
| Denied and missing are both **403** "permission denied, or it may not exist" | Google Cloud does the same; there's no separate visibility rule to keep consistent |
| Not found and not in the tree are **403** before the rate limit, on the failed-auth IP limiter; a missing label is **403** after it. Same body | A caller outside a tree can't spend its budget, and existence doesn't leak |
| The admit cache key includes `resolve_deleted` | Otherwise a `found: false` cached by a read of a deleted node answers the restore route too |
| On the restore route (`resolve_deleted`), a deleted node's own bindings, kept until purge, still count | Google Cloud keeps a deleted project's policy so it can be undeleted. A deleted root has nothing above it to grant the restore |

## Rate limits

| Decision | Why |
|----------|-----|
| A rate-limit policy names the kind it counts at; a route can carry several, and all must pass | A sandbox can't spend a live project's budget, and an org-wide cap still holds. Kubernetes ResourceQuota counts per namespace ([quotas](https://www.kubernetes.io/docs/concepts/policy/resource-quotas/)) |
| Every policy on a route is counted. The headers show the policy with the fewest requests remaining; a **429** carries the longest `Retry-After` among those exceeded | Each bucket sees every admitted request, so one policy's budget doesn't depend on another's. The longest wait is the one after which every policy passes again |
| Monthly and per-plan counts stay in services | They're product rules with product data, not request-rate protection |

## Lists

| Decision | Why |
|----------|-----|
| Node list routes require `[]`; identity filters each node by the labels of that kind's read route | A project-only member has no `project:read` at the org, so a label on the list route would hide projects they can open. Google Cloud's project search returns what the caller has `resourcemanager.projects.get` on ([projects.search](https://docs.cloud.google.com/resource-manager/reference/rest/v3/projects/search)) |
| A deleted node is listed under `?state=deleted` only when every node above it is live | Admit counts a deleted node's own bindings only as the last of a live chain, and such a node can't be restored before its ancestor anyway |
| The access list has one entry per binding that counts at the node, keyed by binding id, with where it sits. On a root it includes guests' bindings below, marked `guest: true` | A member can hold several bindings that count, each with its own role and expiry. GitHub lists outside collaborators on the org |
| Lists show what's in force: bindings past `expires_at` are left out; the access list also leaves out bindings whose role grants nothing where they sit | Not what the sweeper hasn't reached yet |
| `/members/{id}/nodes` items add `root_slug` and `root_label`; without `?kind` they return every kind | A console showing "Gruow / gatheroom" needs the org's name without a second call |
| Response shapes for bindings, access entries, members, service accounts, keys and invites are in the identity API; key lists include revoked keys | One place for each shape |
| Member routes on a node that isn't a root, or on the sign-in node, are **404**; `/members/{id}` routes on a member that isn't a directory member are **404** | Each addresses something that can't exist there |

## What services see

| Decision | Why |
|----------|-----|
| Every request Plat5's gateway admits carries the node ids, the caller's member id (`X-Plat5-Member-Id`) and `X-Plat5-Principal`. Never a person's id | Authorship needs the caller, and operators need to act as someone. Gateways like Kong, Identity-Aware Proxy and Cloudflare Access pass the caller through too; Plat5 differs only in never passing a cross-tree id |
| The member id is per tree, and the directory member id is sent only on sign-in routes | Two orgs can't join their data on a shared person. Google Cloud and Kubernetes show the same identity everywhere; Plat5 differs on purpose ([boundaries](boundaries.md#each-tree-is-a-closed-world)) |
| Records store `X-Plat5-Member-Id` as their author when logic needs it ("published by", "you can't approve your own change") | Separation of duties is a service comparing two member ids on two records: a state rule, not an access check |
| Removed is final: re-adding someone makes a new member id; the old member stays, and its lookup returns its name and `status: removed` | A changelog can show "Dana (former member)", and two stints in an org stay distinct |
| A member stores its principal kind (`members.principal`) | The member shape names `person` or `service_account`, and a removed service account or deleted account has no principal row left |
| Services do no auth, so any front door can call them | The operator plane and internal jobs call the same service API under their own documented rules |
