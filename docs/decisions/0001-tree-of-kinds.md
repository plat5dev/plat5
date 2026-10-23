# 0001: A tree of kinds from config

- **Status:** Accepted. Apply details superseded by [0006](0006-apply-dry-run-and-preconditions.md)
- **Date:** Oct 2026

Plat5 v1 had two fixed realms (the directory and organizations). e10s v2 needed projects inside orgs, with keys, roles, rate limits and audit scoped to a project, without each service inventing its own access model. This record is the model chosen, the config shape, the alternatives, and how each rule compares with Google Cloud and Kubernetes.

## Summary

**Locked.** Plat5 v2 owns a tree of scopes declared in config; e10s owns everything inside a project. Both cut over hard: new tables, no backward compatibility, data migrated by hand.

- **Tree:** directory, organization and project are kinds in config. A node holds only id, kind, parent, slug, label and status.
- **Access:** the union of a principal's bindings on a node and its ancestors. Org members see no projects by default; a role bound at a project grants that project, bound at the org grants all of them. People outside the org can be guests on a project.
- **Credentials:** a person's credential reaches every tree they're bound in, with the binding picked per request. A service account is created at a node and can hold bindings anywhere in its own org's tree.
- **Paths:** name the chain of slugs from the root, for example `/orgs/{organization}/projects/{project}/…`. The gateway resolves slugs to ids; services see only ids.
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

services:
  e10s:
    rate_limits:
      evals-project: { requests: 100, window_seconds: 1, count_at: project }
      evals-org:     { requests: 300, window_seconds: 1, count_at: organization }
```

Labels and numbers are placeholders. Rate-limit policies live on each service.

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
