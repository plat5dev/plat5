# Tree and kinds

Why Plat5 owns a tree whose kinds the deployment declares, and why nodes, ids and slugs are shaped as they are. The rules: [`concepts/model.md`](../concepts/model.md), [`reference/config/kinds.md`](../reference/config/kinds.md), [`reference/api/edge-catalog.md`](../reference/api/edge-catalog.md).

Google Cloud's resource hierarchy is the closest match to the tree ([Resource Manager](https://docs.cloud.google.com/resource-manager/docs/resource-manager-overview)); Kubernetes custom resources are the closest match to declaring kinds in config ([custom resource definitions](https://v1-36.docs.kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/_print/)).

## A tree of kinds from config

Every scope is built from config, so no scope is special: the directory, organizations and projects are all kinds. Plat5 owns the tree (ids, parents, who can reach each node); services own everything inside a node. A product needing projects inside orgs, with keys, roles, rate limits and audit scoped to a project, gets them without each service inventing its own access model.

| Decision | Why |
|----------|-----|
| Kinds are declared in config; no kind is special to Plat5 code | A fixed tree with configurable names is simpler to build, but gives up "no scope is special". Kubernetes declares kinds the same way, with custom resource definitions |
| The directory is a node in the tree: the sign-in kind's one root | Google Cloud keeps identities outside the hierarchy, and Kubernetes stores no users. Making the directory a node is what keeps every scope one mechanism |
| A node holds `id`, `kind`, `parent`, `root`, `slug`, `label`, `status`, nothing else | Everything else about a node is service data keyed by its id. Google Cloud likewise keeps a project's resources in services, not on the project |
| A thing belongs in the tree if a key, a role, a rate limit or an audit filter would ever attach to it | Projects pass. Plans, features and subscribers don't: they're service data keyed by node id |
| A project is a node. Plat5 creates, labels, deletes and restores it; the service keeps everything inside, keyed by the node id | Keys, roles, rate limits and audit all attach to projects, and one implementation of each serves every service |
| The config is a few explicit fields per kind (`parent`, `label`, `sign_in`, `segment`, `name_claim`), with everything else a fixed rule and nonsense refused at apply | Readable and reviewable in a diff, with no hidden behaviour. Presets need a release for a new behaviour; a Zanzibar-style schema language pulls Plat5 toward per-resource permissions, which it doesn't do |
| Kind names can't be `public` or `member` | `public` is a scope; `member` would clash with `member_id`, `X-Plat5-Member-Id` and the `{member}` param |
| A kind's `segment` has the slug shape `^[a-z][a-z0-9-]{0,62}$`. The default is the kind name with `_` turned into `-`, plus `s` | Edge paths look consistent, written or derived. Headers already map `_` to `-` |
| `label` and `name_claim` are at most 128 characters | Same limit as other names |
| The sign-in node's label is its kind's `label`, or the kind name | A sign-in kind often has no label |

## Ids, slugs and labels

A node has a generated id, a slug fixed at creation, and a label that can change, as a Google Cloud project has a project number, a project ID fixed after creation, and a name ([project identifiers](https://docs.cloud.google.com/compute/docs/projects)), and a Kubernetes object has a UID and a name ([names](https://www.kubernetes.io/docs/concepts/overview/working-with-objects/names/)).

| Decision | Why |
|----------|-----|
| Every node but the sign-in node has a slug; slugs route | Machines put slugs in a config file once and they never change; people get readable console URLs; services store ids only, with slug handling once, in Plat5 |
| Slug shape: lowercase letters, digits and hyphens, starting with a letter, not ending with a hyphen, 3 to 63 characters | Google Cloud project IDs' shape ([Project](https://docs.cloud.google.com/php/docs/reference/cloud-resource-manager/latest/V3.Project)), with Kubernetes' 63-character name limit. A slug never contains `_`, so a path segment with `_` is always an id |
| Slugs are unique per parent and kind; roots share one namespace | A person's credential reaches every org they're in, so org slugs must be deployment-wide; project slugs only need to be unique in their org |
| Slugs are never reused within a parent; a purged node keeps a tombstone; a slug can be released by hand | Google Cloud never reuses a deleted project's ID ([delete and restore](https://docs.cloud.google.com/resource-manager/docs/delete-restore-projects)). A machine holding an old slug never reaches a stranger's node |
| A slug omitted on create is generated from the label: lowercased, other characters to `-`, trimmed, filled or prefixed to fit the shape, `-2`, `-3` on collision | Readable, and always valid |
| `me` isn't in the example `reserved_slugs` | Slugs are at least 3 characters, so `me` can never be one |

## Paths name the chain

| Decision | Why |
|----------|-----|
| Paths carry the chain of slugs from the root: `/orgs/{organization}/projects/{project}/…`. Node ids are accepted in any position | A person's credential reaches every org they belong to, and project slugs are unique only within an org, so the org has to be in the path. Kubernetes nests a namespace the same way (`/namespaces/{ns}/pods/{name}`), as GitHub addresses `/{owner}/{repo}` |
| The gateway resolves slugs to ids and forwards only ids, with every ancestor in the subject | Services store ids only. `{subject.organization_id}` is there for a service that wants it |

## Generated routes

Identity's and audit's routes are generated from the kinds into routes files like any other ([edge catalog](../reference/api/edge-catalog.md)), then edited like any other.

| Decision | Why |
|----------|-----|
| Identity's own API addresses everything by id: `/nodes/{id}/…`, `/roots/{kind}`, `/members/{id}/…`. A service account is addressed by its member id | Any front door can call it without slugs. Slugs exist only at the gateway edge |
| Identity serves every upstream the generated catalog names, with the documented status, and its filtered lists read the generated read and delete labels | "Served" means reachable through the catalog with the ids the gateway fills, not just mounted |
| The generated catalog's URLs are `http://identity:3000` and `http://audit:3002` | The compose defaults; the files are edited anyway |
| Filtered lists read the labels of routes in the service named `identity`; identity's routes file must define that service | Identity must know which routes are its own, and the generated catalog names it `identity` |

How the generated catalog is kept honest: [config and apply](config-and-apply.md#one-definition-copies-checked).
