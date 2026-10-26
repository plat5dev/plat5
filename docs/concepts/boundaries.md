# Boundaries

What Plat5 is for, what it won't do, and the ways it can grow without changing the model.

## What Plat5 is for

Products where people work together in shared spaces, whose tenancy boundary is a small, stable set of kinds (org, project, maybe environment): B2B SaaS, developer platforms and APIs, team workspaces in consumer apps, internal platforms. Nodes come and go freely by API; kinds rarely change.

Not for permissions on individual records inside a node, or hierarchies end users invent and reshape. For those, a flat tree plus the product's own tables is lighter, or the policy hook below nodes once it ships.

## Not in Plat5

Do not add these because they would be convenient:

- Per-record permissions inside a node in the gateway, or a policy engine (Topaz, OPA, …) in the default stack. The hook is deferred and is for permissions below a node only
- Deny rules, or any check other than the ancestor rule and route permissions
- Role or permission names or meanings in Plat5 code. Identity's and audit's default permissions are defaults the deployment overrides
- Telling services a principal id, a user id on a tree route, or the caller's permissions or role
- A tree's data naming another tree, a service account bound outside its own tree, or a service account on a user route
- A customer route that lists or searches people, or SMTP in identity
- Global or platform-admin service accounts
- Operator headers, roles or rules in base Plat5. Each other front door owns its own, in its own repo
- Hard deletes of nodes, reusing an id, or reusing a root's slug automatically
- An org settings bag, or limits stored as per-org data
- Cost weights, multiple windows, burst or calendar quotas, or billing fields on rate limits
- Credentials narrower than their principal. A narrower machine is a service account with fewer bindings
- A second writer of the tree. A service that needs nodes creates them through the node API
- Per-tree sessions. The IdP token works everywhere
- Moving a node to another tree (an extension point, below)
- Comparing the caller to the role it grants (a grant cap), or to the member it acts on
- More than one binding per member per node
- Roles in the gateway. The control plane stores the roles document; only identity reads it
- A projection store between the control plane and its readers. Postgres is the one store
- Login UI, passwords or JWKS. That's the IdP
- HSTS at the gateway. TLS is the edge's
- Last-owner guards. An org without an admin is recovered by an operator plane
- Display-name overrides per tree. A person's name comes from the IdP
- Lifecycle events derived from audit

## Extension points

None of these touches nodes, the ancestor rule, slugs, or what services see; they add principal types or change how admit unions bindings.

| Case | Today | Extension |
|---|---|---|
| One outside person on one project | Guest bindings | — |
| A whole org shared into a project; an agency across client orgs | Not possible | Group principals: a binding whose principal is "members of org A" |
| Anyone with a link, or any signed-in user | Not possible | Public principals, like `allUsers` and `allAuthenticatedUsers` |
| Moving a project to another org | Refused | Cross-org move with a `node.moved` event |
| A kind optional in the middle (a project under an org or a folder) | One parent per kind | A list of parents, additive |
| Access per folder, with folders inside folders | Folders are service data inside a node, nested as deep as the service likes; access is decided at the node | A kind allowed as its own parent, with a depth cap |
| Permissions on one record inside a node | Service state rules | The policy hook ([`TODO.md`](../../TODO.md)) |

## Costs accepted knowingly

- Adopting Plat5 means putting the tree under it: routes, path params and "whose data is this" line up with the `kinds` document.
- Moving off Plat5 is a migration of nodes and bindings, not an auth-provider swap.
- Services see a per-tree member id on tree routes, and a user id only on user routes, so a person can't be correlated across orgs from tree data. That's the one property that's hard to reverse, and it's intended.
