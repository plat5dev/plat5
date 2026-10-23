# 0005: Boundaries, sharing beyond one org, and extension points

- **Status:** Accepted
- **Date:** Oct 2026

The worry: a team adopts Plat5, then needs to share a project with another org and is stuck. This record says what Plat5 is for, and how it grows to cover sharing without changing the tree.

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
| Moving a project to another org | Refused | **Cross-org move** with a `project.moved` event; records authored before the move keep their old member ids, which resolve as former members |
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
