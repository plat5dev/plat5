# Plat5 v2 architecture decisions

> Why Plat5 v2 is shaped this way, with references. Written while planning e10s v2, hence the e10s examples. A record, not a contract: where it and the other docs differ, the other docs win.

Oct 22, 2026 · @Kyle Smith

## Summary

**Locked.** Plat5 v2 owns a tree of scopes declared in config; e10s owns everything inside a project. Both cut over hard: new tables, no backward compatibility, data migrated by hand.

- **Tree:** directory, organization and project are kinds in config. A node holds only id, kind, parent, slug, label and status.
- **Access:** the union of a principal's bindings on a node and its ancestors. Org members see no projects by default; a role bound at a project grants that project, bound at the org grants all of them. People outside the org can be guests on a project.
- **Credentials:** a person's credential reaches every tree they're bound in, with the binding picked per request. A service account is created at a node and can hold bindings anywhere in its own org's tree.
- **Paths:** name the chain of slugs from the root, for example `/orgs/{org}/projects/{project}/…`. The gateway resolves slugs to ids; services see only ids.
- **Closed world:** a tree's data refers only to its own nodes and members. Services get the caller's member id for that tree, never a person's id. Services do no auth, so other front doors (such as the operator plane) can call them under their own rules.
- **Deletion:** always soft for `deletion_grace` (default 30 days), restorable, then purged. Ids and slugs are never reused.
- **Config:** only what a deployment really decides (`parent`, `label`, `sign_in`, reserved slugs, roles, rate limits). Everything else is a fixed Plat5 rule.

The model, its config and change rules come first below; the seven decisions after it are what the model means for e10s.

## Plat5 v2 model: kinds, nodes and bindings

Every scope is built from config, so no scope is special: the directory, organizations and projects are all kinds. Plat5 owns the tree (ids, parents, who can reach each node); services own everything inside a node.

| Concept | What it is | Today's equivalent |
| --- | --- | --- |
| **Kind** | A level of the tree, declared in config | The fixed realms: directory, organization |
| **Node** | One instance of a kind: id, kind, parent, slug, label, status. Nothing else | An organization row; the directory |
| **Principal** | A person (born in the directory) or a service account (born at an org or project node). Its id never leaves Plat5 | Same |
| **Member** | A principal's presence in one tree: one per principal per tree, kept after removal. Services see it as the caller (`X-Plat5-Member-Id`); in the directory it's the user id | A membership |
| **Binding** | A role for a member on one node of its tree; a member can have many | None |
| **Route scope** | `scope: <kind>` addresses a node of that kind; `scope: public` has no subject | `organization`, `member`, `user`, `public` |

### Rules

- **Ancestor rule.** A request is allowed when the caller holds the route's label through a binding on the route's node or an ancestor. Access is the union of those bindings; there are no deny rules.
- **People.** A person's credential (IdP token or personal API key) reaches every tree they're bound in; the gateway picks the binding per request. A personal key acts as the person everywhere; anything narrower is a service account.
- **Service accounts.** Created at an org or project node and deleted with it. They can hold bindings anywhere in their own org's tree, never outside it. Never in the directory.
- **Joining.** People join the sign-in kind on first sign-in. Root kinds use invites or an admin add. Child kinds bind people already on the root's roster, or guests who redeem an invite below the root.
- **Default-deny below the root.** Joining an org grants its `default_role`. With a `member` role that holds no project labels, members see no projects until a role is bound at a project, or at the org for all projects.
- **Closed world and any front door.** A tree's data refers only to its own nodes and members, and Plat5 never sends a person's principal id. Services do no auth, so any front door on the private network can call them; Plat5's gateway is one, and others (the operator plane, internal jobs) bring their own documented rules.

**The line between Plat5 and services:** a thing belongs in the tree if a key, a role, a rate limit or an audit filter would ever attach to it. Projects pass. Plans, features and subscribers don't; they're service data keyed by node id.

## The config

The config holds only what a deployment really decides; everything else is a fixed Plat5 rule. It's applied to the control plane like routes are today.

```yaml
deletion_grace: 30d
kinds:
  directory:
    sign_in: idp
    name_claim: name
  organization:
    label: Organization
  project:
    label: Project
    parent: organization

reserved_slugs: [new, settings, api, admin, me]

roles:
  admin:     { labels: ["*"], bindable_at: [organization, project] }
  member:    { labels: [organization:read], bindable_at: [organization] }
  developer: { labels: [project:read, catalog:write, subscribers:write], bindable_at: [organization, project] }
  runtime:   { labels: [check], bindable_at: [organization, project] }
  support:   { labels: [project:read, subscribers:write], bindable_at: [organization], max_duration: 7d }
default_role: member
creator_role: admin

rate_limits:
  evals-project: { requests: 100, window_seconds: 1, count_at: project }
  evals-org:     { requests: 300, window_seconds: 1, count_at: organization }
```

Labels and numbers are placeholders.

| Field | What it does |
| --- | --- |
| Kind name | Names the kind in route scopes, subject fields (`{subject.project_id}`), events and audit. Fixed once nodes exist |
| `parent` | The kind (or list of kinds) a node sits under. Omitted for a root |
| `label` | What consoles show. Can change any time |
| `sign_in` | Marks the one kind people sign in to. It's the single-node root (the directory) |
| `reserved_slugs` | Slugs no node may take, deployment-wide |
| `bindable_at` | The kinds a role can be bound at |
| `max_duration` | The longest a binding with this role may last |
| `default_role` | What joining a root grants. Sets default-deny or default-allow below it |
| `creator_role` | What the creator of a node gets on it. Must be bindable at every kind that can be created |
| `count_at` | The kind a rate-limit bucket counts at. A route can list several policies; all must pass |
| `deletion_grace` | How long a deleted node stays restorable before it's purged. One value for the deployment; 30 days by default |
| `name_claim` | On the sign-in kind: the IdP claim that holds a person's name. Defaults to name |

### Fixed rules

| Area | Rule |
| --- | --- |
| Slugs | Every node except the directory has one: lowercase letters, digits and hyphens, starting with a letter and not ending with a hyphen, 3 to 63 characters. Never a node-id shape |
| Slug uniqueness | Unique per parent and kind; roots share one namespace, so org slugs are deployment-wide |
| Slug reuse | Never, within the same parent. Plat5 keeps a tombstone (slug, id, kind, parent); a slug can be released by hand |
| Slug generation | Omitted on create: generated from the label, with `-2`, `-3` on collision |
| Subject | Each node in the chain as `{kind}_id`, and the caller as `member_id`, in the path or as `X-Plat5-*` headers. Never a person's principal id |
| Labels | Every route states its labels. `required_labels: []` is allowed on purpose and means any member of the tree |
| Denied or missing | 403 "permission denied, or it may not exist" for both, as Google Cloud does. There's no separate visibility rule |
| Node lists | Filtered by Plat5 using the list route's own labels. Services never filter by caller |
| Creator | Gets `creator_role` on the new node unless already covered by inherited bindings. A node created with no member behind the request gets no binding |

## Alternatives considered for the config

Chosen: a few explicit fields, with everything else fixed in Plat5 and nonsense combinations refused at apply.

| Option | What it looks like | For | Against |
| --- | --- | --- | --- |
| **Explicit fields** (chosen) | Each kind sets `parent`, `label`, `sign_in`; the rest is fixed | Readable, reviewable in a diff, no hidden behaviour | Some combinations are nonsense and must be rejected at apply |
| Presets | Each kind picks `directory`, `tenant` or `container` | Short; bad combinations can't exist | A fourth behaviour needs a Plat5 release |
| A permissions schema language (Zanzibar style) | `definition project { relation parent: organization; permission check = parent->member }` | Most expressive; covers per-resource sharing | Harder to read; pulls Plat5 toward per-resource permissions, which are out of scope |
| Fixed tree, configurable names | Always directory, org, project; only labels change | Simplest to build | Gives up "no scope is special" |

## Changing the config and deleting things

Destructive changes are allowed when the caller has seen the plan and confirmed it, and every deletion is soft for a configurable grace period, 30 days by default. This takes Kubernetes' rule that a kind can be deleted with its objects ([Kubernetes](https://v1-36.docs.kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/_print/)) and runs it through Google Cloud's rule that a deleted project is restorable for 30 days and its ID never reused ([Google Cloud](https://docs.cloud.google.com/resource-manager/docs/delete-restore-projects)).

### Applying a config change

Every apply produces a plan first, machine-readable so CI can run it.

- **Safe:** applies.
- **Blocked:** refused, listing the nodes or bindings in the way.
- **Destructive:** applies only with a confirmation naming each affected kind.

Tightened rules (a shorter `max_duration`, a new reserved slug) apply to new values only, as Kubernetes validation ratcheting does ([KEP-4008](https://www.kubernetes.dev/resources/keps/4008/)).

| Change | Class |
| --- | --- |
| Add a kind at the bottom (`environment` under `project`) | Safe |
| Add a kind in the middle (`folder` between org and project) | Safe, in two steps: add `folder` under `organization`, then widen `project`'s parent to `[organization, folder]` |
| Widen a kind's allowed parents | Safe |
| Narrow a kind's allowed parents | Blocked while any node sits under the parent being removed |
| Remove a kind | Destructive. Cascades to its nodes and their descendants; kinds that name it as parent must go or be re-parented in the same apply |
| Rename a kind | Not supported; change `label` instead |
| Change `label`, add a reserved slug | Safe |
| Remove `bindable_at` kinds from a role, or remove a role | Safe. Bindings keep the role's name and grant nothing until it's back, as with a deleted Google Cloud custom role |

### Deleting nodes

- **Soft for deletion\_grace, 30 days by default.** The gateway refuses the node at once. Children cascade.
- **Three events:** `<kind>.deleted` (services stop serving, keep data), `<kind>.restored` (services resume), `<kind>.purged` (services delete data). Background jobs check node state; the gateway only guards requests.
- **Self-serve restore** within the grace period by anyone who could delete it; deleted nodes are listed with `?state=deleted`. A deleted kind comes back by re-applying it, an operator action.
- **Keys stay revoked.** Nodes, bindings and service accounts come back; keys don't, so one that leaked near the deletion stays dead.
- **After the grace period, purged.** Ids and slugs are never reused; the tombstone lets operators answer what used to exist.

### Moving nodes

- **Within an org** (between folders, once a middle kind exists): allowed. Inherited roles switch to the new parent, as Google Cloud does when a project moves ([Resource Manager](https://docs.cloud.google.com/resource-manager/docs/resource-manager-overview)).
- **Across orgs:** refused. The org is the isolation boundary.

## How the locked rules compare with the references

Most rules match Google Cloud or Kubernetes directly; two depart on purpose, both because of the closed-world rule.

| Rule | Reference | Same or different |
| --- | --- | --- |
| Access is the union of bindings on the node and its ancestors; no deny rules | Google Cloud's effective policy is the union of inherited policies ([IAM hierarchy](https://docs.cloud.google.com/iam/docs/resource-hierarchy-access-control)); Kubernetes RBAC is additive and a ClusterRole can be bound in one namespace ([RBAC](https://kubernetes.io/rbac/)) | Same |
| Default-deny below the org, set by `default_role` | GitHub base permissions set what every org member gets on every repository, and can be none; a higher grant on one repository wins ([GitHub](https://docs.github.com/en/enterprise-server@3.17/organizations/managing-user-access-to-your-organizations-repositories/managing-repository-roles/setting-base-permissions-for-an-organization)) | Same |
| Node lists filtered to what the caller can read | Google Cloud's project search returns projects the caller has `resourcemanager.projects.get` on ([projects.search](https://docs.cloud.google.com/resource-manager/reference/rest/v3/projects/search)). Kubernetes can't filter lists this way | Same as Google Cloud |
| Id, fixed slug, editable label | Google Cloud project number, project ID (fixed after creation) and name ([projects](https://docs.cloud.google.com/compute/docs/projects)); Kubernetes UID and name ([names](https://www.kubernetes.io/docs/concepts/overview/working-with-objects/names/)) | Same |
| Slug shape: lowercase letters, digits, hyphens, starting with a letter | Google Cloud project IDs: 6 to 30 such characters, start with a letter, no trailing hyphen ([Project](https://docs.cloud.google.com/php/docs/reference/cloud-resource-manager/latest/V3.Project)) | Same shape; 3 to 63 characters, Kubernetes' name limit |
| Slugs never reused | Google Cloud never reuses a deleted project's ID; Kubernetes allows a name again after deletion | Google Cloud's choice |
| 30-day soft delete, restorable | Google Cloud projects ([delete and restore](https://docs.cloud.google.com/resource-manager/docs/delete-restore-projects)) | Same |
| Kinds declared in config; deleting a kind deletes its nodes | Kubernetes custom resource definitions | Same, but soft and confirmed |
| Expiring bindings, role duration caps | Google Cloud expiring access ([temporary access](https://docs.cloud.google.com/iam/docs/configuring-temporary-access)); Access Approval for operator access, later ([Access Approval](https://docs.cloud.google.com/assured-workloads/access-approval/docs/approve-requests)) | Same |
| Rate limits counted at a kind | Kubernetes ResourceQuota per namespace ([quotas](https://www.kubernetes.io/docs/concepts/policy/resource-quotas/)) | Same idea |
| One personal credential across trees | A cloud console login works in every project the user has a role in | Same |
| Service accounts born at a node, bound anywhere in its org | Google Cloud service accounts live in one project and can be used in others ([cross-project](https://docs.cloud.google.com/eventarc/docs/use-cross-project-service-account)) | Same, limited to one org |
| Services see a per-tree member id, never the person's id | Google Cloud and Kubernetes show the same identity everywhere | **Different on purpose:** a person can't be correlated across orgs |
| The directory is a node in the tree | Google Cloud keeps identities outside the hierarchy; Kubernetes stores no users | **Different on purpose:** it's what makes no scope special |

## References

Google Cloud's resource hierarchy is the closest match to this design; Kubernetes custom resources are the closest match to declaring kinds in config.

| Reference | What it shows | What Plat5 takes from it |
| --- | --- | --- |
| [Google Cloud Resource Manager overview](https://docs.cloud.google.com/resource-manager/docs/resource-manager-overview) | Organization at the root, folders, then projects; every resource belongs to exactly one project; policies set higher up are inherited below; moving a project changes what it inherits | The tree, downward inheritance, and moves within an org |
| [Google Cloud project identifiers](https://docs.cloud.google.com/compute/docs/projects) | Project name: editable, not unique. Project ID: globally unique, editable only at creation, then permanent. Project number: generated | `label` (editable) vs `slug` (fixed at creation) vs node id (generated) |
| [Google Cloud delete and restore projects](https://docs.cloud.google.com/resource-manager/docs/delete-restore-projects) | A deleted project is unusable at once, restorable for 30 days, and its ID can't be reused | Soft delete, 30 days by default; slugs never reused |
| [Google Cloud folders.delete](https://docs.cloud.google.com/resource-manager/reference/rest/v3/folders/delete) | Only an empty folder can be deleted; it's removed about 30 days later | Narrowing a parent is refused while nodes depend on it |
| [Kubernetes custom resource definitions](https://v1-36.docs.kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/_print/) | Deleting a definition deletes every object stored in it; re-creating it starts empty | A kind can be deleted with its nodes, behind a plan and a confirmation |

As of Oct 22, 2026. Not yet checked: how relationship-based permission engines such as SpiceDB validate schema changes against stored data. Worth reading before writing Plat5's apply validation.

## How a check flows in v2

1. The gateway matches the route (`scope: project`) and reads the credential.
2. **New:** it resolves the slug chain (`{organization}`, `{project}`) to node ids.
3. Identity's admit answers for that credential on that chain: the member id, the labels from the union of bindings on the project and its ancestors, and a `good_until`. The answer is cached like credentials are.
4. The gateway checks the route's labels, then each rate-limit policy.
5. The request goes upstream with `X-Plat5-Organization-Id`, `X-Plat5-Project-Id`, `X-Plat5-Member-Id` and `X-Plat5-Principal`.

The one new step resolves the project slug to its node and checks that the key's binding sits on that project or above it. Every fact it needs is in Plat5, so there's no outside call; the result is cached like credentials are.

## Decision list

Seven decisions follow from the model. Six land in Plat5 v2 as part of the model itself; only authorship stays inside e10s. Change the dropdowns as you decide.

| # | Decision | Plat5 change | Recommendation | Status |
| --- | --- | --- | --- | --- |
| 1 | How a key is limited to one project | Needed | A service account bound to the project; the gateway checks bindings on the project and its ancestors. Members see no projects by default | Decided |
| 2 | How the path names the project | Needed | `/orgs/{org}/projects/{project}/…` by slugs; the gateway resolves them and forwards ids | Decided |
| 3 | What a project is | Needed | A Plat5 node; e10s keys its data on the node id | Decided |
| 4 | Rate limits per org or per project | Needed | Each limit names the kind it counts at; monthly limits stay e10s counts | Decided |
| 5 | Per-project activity log | Needed | Audit events carry the node; filter by project | Decided |
| 6 | Who published or approved a version | None | Records store `X-Plat5-Member-Id` as their owner | Decided |
| 7 | What may enter a tree's data | Needed | Each tree is a closed world; people cross, data doesn't; bindings can expire | Decided |

## 1. Project-scoped keys

Locked: a key is exactly as wide as its service account's bindings, and a binding can sit on a project. A sandbox's machine key belongs to a service account bound only to the sandbox project, so it reaches that project and nothing else. Org members see no projects until a role is bound at a project, or at the org for all of them.

- **People** stay on the org's roster. A role at the org reaches every project below it; a role at one project reaches only that project.
- **Service accounts** can be born in a project. Deleting the project revokes and deletes them with their keys.
- **The gateway's check** is the ancestor rule: the key's binding must sit on the route's project or above it. Plat5 stores every fact it needs, so there's no outside call and no new failure mode.
- **The policy-engine hook** stays deferred, for per-resource permissions only, such as sharing one document. Projects don't need it.

This replaces the earlier options (org-wide keys, the hook, a project stored on the key, e10s checking the caller, an org per project): bindings give a narrow key without making a credential narrower than its membership.

Decided: a project role can exceed the org role. Access is the union of bindings on the node and its ancestors.

## 2. The path names the org and the project

Locked: paths carry the chain of slugs from the root. A person's credential reaches every org they belong to and project slugs are unique only within an org, so the org has to be in the path. The gateway resolves each slug to its node id and forwards only ids. Node ids are accepted in any position too.

```yaml
routes:
  - path: /orgs/{organization}/projects/{project}/subscribers/{subscriber_id}/entitlements/check
    scope: project
    upstream: /projects/{subject.project_id}/subscribers/{path.subscriber_id}/entitlements/check
    methods:
      POST:
        required_labels: [check]
        audit: false
```

This is how Kubernetes nests a namespace in its paths (`/namespaces/{ns}/pods/{name}`) and how GitHub addresses `/{owner}/{repo}`. The subject carries every ancestor, so `{subject.organization_id}` is there for services that want it.

- **Machines** put the org and project slugs in a config file once; they never change.
- **People** get readable console URLs.
- **Services** store ids only; slug handling lives once, in Plat5.

## 3. A project is a Plat5 node

Recommendation: Plat5 creates, labels, deletes and restores projects; e10s keeps everything inside one, keyed by the node id. The node holds only id, kind, parent, slug, label and status, so a project's catalog, subscribers and settings stay e10s data.

- **Create:** the console calls Plat5's route. Plat5 emits `project.created` and gives the creator `creator_role` on it; e10s creates its project-settings row from the event.
- **Delete:** soft for the grace period. The gateway refuses the project at once. On `project.deleted` e10s stops serving it but keeps its data; on `project.restored` it resumes; on `project.purged` it deletes the data.
- **Org delete:** cascades to its projects, each with its own events.

Still to decide: does e10s keep any project data that must exist before the first event arrives (default plan settings, for instance), or can it always create rows lazily?

## 4. Rate limits and metering

Recommendation: a rate-limit policy names the kind it counts at, so the burst limit on eval routes can count per org or per project. Monthly evals and subscribers stay e10s counts: org-wide against the org plan, broken down per project.

| Limit | Where it's counted | Org-wide | Per project |
| --- | --- | --- | --- |
| Burst, 100/s on eval routes | Gateway, Valkey | `count_at: organization` | `count_at: project`, so a sandbox can't spend a live project's budget |
| Monthly evals | e10s | Sum across projects; Free's allotment applies to the sum | One counter row per project |
| Subscribers | e10s, at subscriber create | Count across projects against the org plan | Count per project |

Today the gateway's limiter subject is fixed by scope. In the v2 model a route's subject carries every ancestor node, so choosing the level is a policy setting, not a special case.

Decided: both. Eval routes carry a project policy and an org policy, and a request must pass both.

## 5. Per-project activity log

Recommendation: every audit event records the node it touched and that node's ancestors, so "this project" and "this org and everything under it" are both filters. Reading stays behind `organization:audit:read`, or a role at the project for a project's own history.

Audit stays reporting only: product features never read it, so the plan changelog can't be built from it (decision 6).

## 6. Who published or approved a version

Locked: versions, proposals and approvals store the caller's member id, from `X-Plat5-Member-Id`, as their owner. The member id is stable per person per org, so "published by" and "don't notify me about my own change" compare like with like.

- **Any project route can record the owner.** No special binding routes; a service that prefers the id in its path rewrites `{subject.member_id}`.
- **Read** on the same project routes, so the changelog and review queue list everyone's records.
- **Separation of duties** ("you can't approve your own change") is e10s comparing two member ids: the proposal's and the approval's.
- **Departed members:** their records stay; the member lookup returns a removed member's name and status, so the changelog shows "Dana (former member)".
- **Operators** can supply a member id by acting as one, which the operator plane specifies in its own repo; a change made without one records no owner.

## 7. Each tree is a closed world

Locked. **Data inside a tree refers only to things inside it: its nodes and its members. People cross between trees; data doesn't.** No tree's data names another tree or a person's principal id. Plat5 guarantees the second part by never sending person ids; the first is a documented convention for services.

Three rules follow:

- **Authored acts are owned records**, recorded with the member id of whoever made them.
- **Unauthored acts are fine.** Who did them lives in audit, outside service data.
- **Crossing a boundary takes a binding**, which means appearing on the root's access list, as a member or a guest.

**Caller headers are allowed; person ids aren't.** Every request Plat5's gateway admits carries the node ids, the caller's member id (`X-Plat5-Member-Id`) and `X-Plat5-Principal`. The member id is per tree, so two orgs can't join their data on a shared person. Gateways with one identity system per app, such as Kong, Google's Identity-Aware Proxy and Cloudflare Access, pass the caller through too; Plat5 differs only in never passing a cross-tree id. Other front doors, like the operator plane, call the same service APIs under their own documented rules. Access is still decided only at the front door, never by a service reading the caller.

### Stress test

| Case | Holds? | How it works | Verdict |
| --- | --- | --- | --- |
| "Published by Dana" on a version | Yes | The version is owned by Dana's member id | Fine |
| CI publishes the catalog from the repo | Yes | The CI service account is a binding in the org | Fine |
| "Don't notify me about my own change" | Yes | The change is an owned record, so its owner is known | Fine |
| Notification preferences per org or project | Yes, and better | Keyed by the person's member id in that org; leaving the org deletes them | The rule improves the design |
| Support grants an override for a customer | Strains | Support must be a member of the customer's org | Fine once bindings can expire |
| Operators fix data during an incident | Yes | Node routes; no author on the record; the operator audit log has the operator | Fine |
| One inbox across all of a person's orgs | Strains | The client asks each org and merges, or entries are copied in as plain values with no ids | Needs a stated pattern |
| An agency managing many client orgs | Breaks | Agency people join each org one by one; "org A manages org B" can't be expressed | Later: group principals (extension points) |
| "Last edited by" on every row | Strains | Each edit becomes a revision record, or the column goes | Cost accepted |

### Additions this needs

- **Bindings that expire.** Support access becomes "the customer adds a support member for 7 days": visible, consented to, and self-ending. Each org chooses whether support members are allowed.
- **A stated pattern for views across trees.** Built by the client from one credential per tree, or copied in as values with no ids. Never references.

The agency case is answered later by group principals: the agency org is bound into each client project or org, so no ids cross trees. One outside person on one project is already covered in v2 by guest bindings.

## Boundaries and extension points

Plat5 is for a stable tenancy boundary enforced outside every handler: the tree is fixed by config, and who can be bound to it is the axis that grows. Sharing across orgs, the most likely "we're stuck" request, is a question of principals, not of the tree.

**What Plat5 is for:** hierarchical B2B products whose tenancy boundary is a small, stable set of kinds (org, project, maybe environment). Nodes come and go freely by API; the kinds rarely change.

**What it isn't for:** permissions on individual records inside a node, and hierarchies end users invent and reshape. For those, a flat org plus the product's own tables is lighter, or the policy hook below nodes.

### Sharing beyond one org

Google Cloud's role bindings name users, service accounts, groups, whole domains, `allAuthenticatedUsers` and `allUsers` ([IAM binding](https://docs.cloud.google.com/dotnet/docs/reference/Google.Cloud.Iam.V1/latest/Google.Cloud.Iam.V1.Binding)). Plat5 grows the same way.

| Case | v2 | Extension |
| --- | --- | --- |
| One outside person on one project | **In v2: guest bindings.** An invite below the org, gated by `organization:guests:write`; guests are listed as such on the org's access list. GitHub's outside collaborators work this way ([GitHub](https://docs.github.com/en/organizations/managing-user-access-to-your-organizations-repositories/managing-outside-collaborators/adding-outside-collaborators-to-repositories-in-your-organization)) | — |
| A whole org shared into a project; an agency across client orgs | Not possible | **Group principals:** a binding whose principal is "members of org A" (Google Cloud's `group:` and `domain:`). Open design point: authored records need a per-person binding, for example one created the first time someone acts through the group |
| Anyone with a link, or any signed-in user | Not possible | **Public principals,** like `allUsers` and `allAuthenticatedUsers` |
| Moving a project to another org | Refused | **Cross-org move** with a `project.moved` event; records authored before the move keep their old binding ids, which resolve as former members |
| Data in one org referring to data in another | Blocked by design | None. Share the container instead: bind people into one project |

None of these extensions touches nodes, the ancestor rule, slugs or what services see; they add principal types and change how admit unions bindings.

### Other extension points

- **Permissions below a node:** the deferred policy hook, unchanged.
- **Nesting a kind under itself** (user folders): a kind allowed as its own parent with a depth cap, nodes past the first level addressed by id. Google Cloud folders nest the same way. Not in v2.
- **Two writers of the tree:** rejected. A service that needs nodes creates them through the node API, so the gateway's check never runs ahead of Plat5.

### Costs accepted knowingly

- Adopting Plat5 means putting the tree under it: routes, path params and "whose data is this" line up with the kinds document. It isn't an SDK drop-in.
- Moving off Plat5 is a migration of nodes and bindings, not an auth-provider swap.
- Services see a per-tree member id, never a person's id. Keeping person ids inside Plat5 is the one property that's hard to reverse, and it's intended.

## Plat5 v2 work this implies

Plat5 v2 is a hard cut like e10s v2: new identity tables for nodes and bindings, data migrated by hand, and the old scopes and older key formats removed.

- [ ] **Config:** kinds (`parent`, `label`, `sign_in`), `reserved_slugs`, roles with `bindable_at` and `max_duration`, `default_role`, `creator_role`, rate-limit policies with `count_at`. Refuse nonsense combinations.
- [ ] **Apply plan:** safe, blocked or destructive per change; confirmation naming each kind; machine-readable output; tightened rules apply to new values only.
- [ ] **Identity:** nodes (id, kind, parent, slug, label, status), bindings with optional `expires_at`, service accounts born at a node and bound anywhere in their org, guest bindings through invites below a root, slug tombstones.
- [ ] **Node API:** create, label, delete, restore, list with `?state=deleted`; creator binding on create; filtered lists; 403 for denied or missing nodes.
- [ ] **Lifecycle events:** `<kind>.created`, `.deleted`, `.restored`, `.purged`; purge after `deletion_grace`; keys stay revoked on restore.
- [ ] **Gateway:** `scope: <kind>` and `scope: public`; slug chain to ids in the admission cache; the person's member picked per request; the ancestor rule over the union of bindings; binding expiry; subject fields and `X-Plat5-*` headers (node ids, member id, principal).
- [ ] **Route rules at apply:** every route states its labels; `[]` means any member of the tree.
- [ ] **Rate limits:** several policies per route, each counting at a kind.
- [ ] **Audit:** events carry the node and its ancestors; reads filter by node.
- [ ] **Remove:** the `public` / `user` / `organization` / `member` scope names, the `user_id` wire name (the directory member id replaces it), per-org sessions, version 1 key formats.
- [ ] **Operator plane (separate repo, its own spec):** browse the tree by kind; release a slug tombstone; restore a deleted kind.
