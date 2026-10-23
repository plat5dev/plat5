# Boundaries

What Plat5 is for, what it won't do, and the ways it can grow without changing the model.

## What Plat5 is for

Hierarchical B2B products whose tenancy boundary is a small, stable set of kinds (org, project, maybe environment). Nodes come and go freely by API; kinds rarely change.

Not for permissions on individual records inside a node, or hierarchies end users invent and reshape. For those, a flat tree plus the product's own tables is lighter, or the policy hook below nodes once it ships.

## Not in Plat5

Do not add these because they would be convenient:

- Per-record permissions inside a node in the gateway, or a policy engine (Topaz, OPA, …) in the default stack. The hook is deferred and is for permissions below a node only
- Deny rules, or any check other than the ancestor rule and route labels
- Role or label names or meanings in Plat5 code. Generated labels are catalog defaults the team edits
- Telling services a person's principal id, or the caller's labels or role
- A tree's data naming another tree, a service account bound outside its own tree, or a service account in the directory
- A customer route that lists or searches people, or SMTP in identity
- Global or platform-admin service accounts
- Operator headers, roles or rules in base Plat5. Each other front door owns its own, in its own repo
- Hard deletes of nodes, reusing an id, or reusing a slug automatically
- An org settings bag, or limits stored as per-org data
- Credentials narrower than their principal. A narrower machine is a service account with fewer bindings
- A second writer of the tree. A service that needs nodes creates them through the node API
- Per-tree sessions. The IdP token works everywhere
- Moving a node to another tree (an extension point, below)
- Comparing the caller to the role it grants (a grant cap), or to the member it acts on
- More than one binding per member per node
- Roles in the gateway. The control plane stores the roles document; only identity reads it
- Login UI, passwords or JWKS. That's the IdP
- Last-owner guards. An org without an admin is recovered by an operator plane
- Display-name overrides per tree. A person's name comes from the IdP

## Extension points

None of these touches nodes, the ancestor rule, slugs, or what services see; they add principal types or change how admit unions bindings.

| Case | Today | Extension |
|---|---|---|
| One outside person on one project | Guest bindings | — |
| A whole org shared into a project; an agency across client orgs | Not possible | Group principals: a binding whose principal is "members of org A" |
| Anyone with a link, or any signed-in user | Not possible | Public principals, like `allUsers` and `allAuthenticatedUsers` |
| Moving a project to another org | Refused | Cross-org move with a `<kind>.moved` event |
| Folders inside folders | Not possible | A kind allowed as its own parent, with a depth cap |
| Permissions on one record inside a node | Service state rules | The policy hook ([`TODO.md`](../../TODO.md)) |

## Costs accepted knowingly

- Adopting Plat5 means putting the tree under it: routes, path params and "whose data is this" line up with the `kinds` document.
- Moving off Plat5 is a migration of nodes and bindings, not an auth-provider swap.
- Services see a per-tree member id, never a person's id, so a person can't be correlated across orgs. That's the one property that's hard to reverse, and it's intended.
