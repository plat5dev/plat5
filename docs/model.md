# Plat5 model

> Plat5's model: what each concept is, the rules that hold everywhere, where each decision lives, and what Plat5 will not do. The other docs are how it shows up on the wire; when they disagree with this page, they are wrong. The reasons and references behind each decision: [`decisions.md`](decisions.md).

Plat5 is the front door for multi-tenant APIs. It owns a tree of nodes whose kinds the deployment declares, the principals who can reach each node, and the check at the gateway. Services own everything inside a node.

On every request the gateway answers three questions: who is calling (the credential), which node the request is about (the path), and whether the caller's bindings on that node or its ancestors grant a label the route requires. It then forwards the request with node ids and the caller's member id. Slugs and person ids never reach a service.

## Concepts

| Concept | What it is |
|---|---|
| **Kind** | A level of the tree, declared in the `kinds` document: `directory`, `organization`, `project`, … No kind is special to Plat5 code ([`control-plane.md`](control-plane.md#kinds)) |
| **Node** | One instance of a kind: `id`, `kind`, `parent`, `root`, `slug`, `label`, `status`. Nothing else. Everything else about a node is service data keyed by its id |
| **Root** | A node with no parent. Each root and the nodes under it are one **tree**: the isolation boundary. Organizations are roots |
| **Sign-in node** | The one root of the kind marked `sign_in` (the directory). Every person has a member there. It has no slug and no bindings |
| **Principal** | A person (born at first sign-in) or a service account (born at a node of an org's tree). Its id never leaves Plat5 |
| **Member** | A principal's presence in one tree: one per principal per tree, kept after removal. The id services see as the caller (`X-Plat5-Member-Id`). In the directory it's the person's user id |
| **Binding** | A role for a member on one node of the member's tree, optionally expiring. A member can have many |
| **Guest** | A member with no binding on its tree's root, only on nodes below it. Created by redeeming an invite below the root. Derived, not stored |
| **Role** | The deployment's name for a set of labels, bindable at some kinds ([`roles.md`](roles.md)). Plat5 names no roles |
| **Label** | A permission a route can require. Only the gateway checks labels |
| **Route scope** | `scope: <kind>` addresses a node of that kind; `scope: public` has no subject ([`routes.md`](routes.md)) |
| **Subject** | What the gateway sends a service: each node's id in the chain, and the caller's member id in that tree |
| **Credential** | An IdP token or a personal API key (people), or a service account key. Belongs to the principal, not to a tree |
| **Front door** | Anything on the private network that calls services. Plat5's gateway is one; an operator plane is another |
| **Audit log** | Per root. Who did what, for reporting and compliance ([`audit.md`](audit.md)) |
| **Lifecycle event** | A birth, delete, restore or purge of a node, or a member joining or leaving a tree ([`identity.md`](identity.md#lifecycle-events)) |

## Principles

1. **The tree is config; nodes are data; no scope is special.**
2. **Plat5 owns the tree and who can reach each node; services own what's inside a node.**
3. **Access is decided only at the gateway,** from the union of a member's bindings on the node and its ancestors.
4. **The path names the node; the credential names the principal.**
5. **Services see node ids and the caller's member id, never a person's id.**
6. **Services do no auth, so any front door can call them.** A service's API is its paths and documented headers, reachable only on the private network. Plat5's gateway is one front door; others (an operator plane, an internal job, a custom gateway) bring their own rules and docs.
7. **The deployment's data, not Plat5's names.** Kinds, roles, labels and routes are all config.
8. **Dangerous changes are allowed once planned and confirmed; deletes are soft.**

## Rules

- **Ancestor rule.** A request is allowed when the caller holds one of the route's labels through a binding on the route's node or an ancestor. Access is the union of those bindings. There are no deny rules.
- **Default-deny below the root.** Joining a root grants `default_role` there. With a default role that holds no labels for child kinds, members see no projects until a role is bound at a project, or at the org for all of them. A deployment that wants default-allow gives its default role those labels.
- **People.** A person's credential (IdP token or personal API key) reaches every tree they're in; the gateway picks the member for the tree in the path, per request. A personal key acts as the person everywhere. Anything narrower is a service account.
- **Service accounts.** Born at a node of an org's tree and deleted with it. They can hold bindings anywhere in their own tree, never outside it, and never in the directory.
- **Joining.** People join the directory at first sign-in. A root is joined by invite or by an admin adding someone. Nodes below a root bind people already in the tree, or guests who redeem an invite below the root.
- **Closed world.** A tree's data refers only to its own nodes and members. Plat5 guarantees half of this by never sending a person's principal id; the other half is a convention services follow. People cross trees; data doesn't.
- **Denied or missing.** A node you can't use and a node that doesn't exist get the same **403** "permission denied, or it may not exist", as Google Cloud answers. There's no separate visibility rule.
- **Node lists.** Plat5 filters node lists by the list route's own labels, so `GET /orgs/{organization}/projects` returns only the projects the caller can read. Services never filter by caller.
- **Deletion.** Soft for `deletion_grace` (30 days by default), restorable, then purged. Ids and slugs are never reused.
- **Removed is final.** Re-adding someone to a tree makes a new member id. The old member stays, and a lookup returns its name and `status: removed`.

## Where each decision lives

| Question | Decided by |
|---|---|
| Who is this? | The IdP or identity, from the credential |
| Which node? | The path, resolved by the gateway from slugs or ids |
| May they call this route on this node? | The gateway: the route's labels against the union of the member's bindings on the node and its ancestors |
| Which nodes can they see? | Plat5, filtering node lists by the list route's labels |
| May they touch this record inside a node? | The service's state rules today; the deferred policy hook later ([`../TODO.md`](../TODO.md)) |
| Is this valid for the resource's current state? | The service |
| Who did it? | The record, when logic needs it (the member id as author or owner); audit otherwise |

## Where a field goes

When a service is tempted to store a member id, ask what should happen to it if that member left tomorrow.

| Answer | It's… | It lives in |
|---|---|---|
| Nothing; it's a record of what happened | History | Audit |
| Someone else takes it over | A relationship, like assignee or reporter | Service data. Set by the client, governed by edit permission |
| It stays theirs | Authored, like a comment, an approval or a published version | Service data with `X-Plat5-Member-Id` as its author. A lookup shows a former member's name |
| It goes away with them | Owned, like a subscription or a notification setting | Service data keyed by the member id. Cleaned up on `member.removed` |

**Record the member id when logic depends on who did it; never decide access from it.** The gateway already decided. Separation of duties ("you can't approve your own change") is a service comparing two member ids on two records: a state rule, not an access check. Product features never read audit.

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
- Hard deletes of nodes, or reusing an id or a slug
- An org settings bag, or limits stored as per-org data
- Credentials narrower than their principal. A narrower machine is a service account with fewer bindings
- A second writer of the tree. A service that needs nodes creates them through the node API
- Per-tree sessions. The IdP token works everywhere
- Moving a node to another tree (an extension point, below)

## Extension points

None of these touches nodes, the ancestor rule, slugs, or what services see; they add principal types or change how admit unions bindings.

| Case | Today | Extension |
|---|---|---|
| One outside person on one project | Guest bindings | — |
| A whole org shared into a project; an agency across client orgs | Not possible | Group principals: a binding whose principal is "members of org A" |
| Anyone with a link, or any signed-in user | Not possible | Public principals, like `allUsers` and `allAuthenticatedUsers` |
| Moving a project to another org | Refused | Cross-org move with a `<kind>.moved` event |
| Folders inside folders | Not possible | A kind allowed as its own parent, with a depth cap |
| Permissions on one record inside a node | Service state rules | The policy hook ([`../TODO.md`](../TODO.md)) |

## Costs accepted knowingly

- Adopting Plat5 means putting the tree under it: routes, path params and "whose data is this" line up with the `kinds` document.
- Moving off Plat5 is a migration of nodes and bindings, not an auth-provider swap.
- Services see a per-tree member id, never a person's id, so a person can't be correlated across orgs. That's the one property that's hard to reverse, and it's intended.
