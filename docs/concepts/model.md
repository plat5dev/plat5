# Model

> The concepts and principles everything else builds on. When another doc disagrees with this page, the other doc is wrong. Why each decision was made: [decisions](../decisions/README.md), by topic.

Plat5 is the front door for multi-tenant APIs. It owns a tree of nodes whose kinds the deployment declares, the principals who can reach each node, and the check at the gateway. Services own everything inside a node.

On every request the gateway answers three questions: who is calling (the credential), which node the request is about (the path), and whether the caller's bindings on that node or its ancestors grant a permission the route requires. It then forwards the request with node ids and the caller's member id. Slugs and principal ids never reach a service.

## Concepts

| Concept | What it is |
|---|---|
| **Kind** | A level of the tree, declared in the `kinds` document: `organization`, `project`, … Each has one parent kind, or none for a root. No kind is special to Plat5 code ([`reference/config/kinds.md`](../reference/config/kinds.md)) |
| **Node** | One instance of a kind: `id`, `kind`, `parent_id`, `root_id`, `slug`, `display_name`, `status`. Nothing else. Everything else about a node is service data keyed by its id |
| **Root** | A node with no parent. Each root and the nodes under it are one **tree**: the isolation boundary. Organizations are roots |
| **Chain** | A node and its ancestors, root first. Each kind has one chain of kinds, so each node has one path |
| **Principal** | A person (created at first sign-in, or when an admin adds them) or a service account (belongs to a node of an org's tree). Its id never leaves identity |
| **User** | A person as their own routes see them. A person's user id is sent only on user routes (`X-Plat5-User-Id`) and in user events, never with a tree |
| **Member** | A principal's presence in one tree: one per principal per tree, kept after removal. The id services see as the caller on tree routes (`X-Plat5-Member-Id`) |
| **Binding** | A role for a member on one node of the member's tree. A member can have many |
| **Guest** | A person's member with no binding on its tree's root, only on nodes below it. Created by redeeming an invite below the root. Derived, not stored. A service account is never a guest |
| **Role** | The deployment's name for a set of permissions, bindable at some kinds ([`concepts/access.md`](access.md)). Plat5 names no roles |
| **Permission** | What a route can require, and what a service asks identity about outside a request. Only Plat5 decides who holds one |
| **Route scope** | `scope: <kind>` addresses a node of that kind; `scope: user` is the caller as a person, with no node; `scope: public` has no caller ([`reference/config/services.md`](../reference/config/services.md)) |
| **Credential** | An IdP token or a personal API key (people), or a service account key. Belongs to the principal, not to a tree |
| **Front door** | Anything on the private network that calls services. Plat5's gateway is one; an operator plane is another |
| **Audit log** | Per root, and per person for their user routes. Who did what, for reporting and compliance ([`concepts/audit.md`](audit.md)) |
| **Lifecycle event** | What's created and what ends, in two feeds: tree events (a node created, deleted, restored or purged; a member joining or leaving a tree) and user events (a person coming to exist; an account deleted) ([`concepts/lifecycle.md`](lifecycle.md#lifecycle-events)) |

## Ids and slugs

Every id is a ULID, lowercase, with no type prefix: `01jbx9q3t5vk8m2n4p6r7s8t9w`. Ids are stored, returned and accepted only in that form; an uppercase id isn't canonical and matches nothing. Secrets keep a prefix so leak scanners can find them: API keys (`{brand}-k-2-…`) and invite tokens (`inv_…`). A principal's id never leaves identity.

Every node has a slug, fixed at creation. Fixed rules, not config:

| Rule | |
|------|--|
| Shape | Lowercase letters, digits and hyphens; starts with a letter, doesn't end with a hyphen; 3 to 63 characters. Never an id: ids start with a digit |
| Uniqueness | Per parent and kind. Roots share one namespace, so org slugs are deployment-wide |
| Reuse | A node's slug is free again once the node is purged. A root's slug never is: a purged root keeps its row as a tombstone (slug, id, kind). An operator can release a purged root's slug ([slug release](../reference/api/identity.md#slug-release)) |
| Generation | Omitted on create: generated from the display name, lowercased, with each run of other characters as `-`, trimmed. Prefixed with the kind name when it would start with a digit or be too short (`project-42`). `-2`, `-3` on collision; reserved slugs and root tombstones count as taken |
| Reserved | `reserved_slugs` in `plat5.yml`. **422** |

A path segment that starts with a digit is read as a node id, anything else as a slug. Both resolve under the previous node.

## Principles

1. **The tree is config; nodes are data; no scope is special.**
2. **Plat5 owns the tree and who can reach each node; services own what's inside a node.**
3. **Access is decided only by Plat5,** from the union of a member's bindings on the node and its ancestors. Front doors enforce it during requests; services enforce it outside them, by asking identity.
4. **The path names the node; the credential names the principal.**
5. **Services see node ids and the caller's member id on tree routes, and a user id only on user routes and in user events. Never a principal id.**
6. **Services do no auth, so any front door can call them.** A service's API is its paths and documented headers, reachable only on the private network. Plat5's gateway is one front door; others (an operator plane, an internal job, a custom gateway) bring their own rules and docs.
7. **The deployment's data, not Plat5's names.** Kinds, roles, permissions and routes are all config.
8. **Config changes are planned before they apply; deletes are soft.**
9. **Defaults, not limits.** Where reasonable teams would choose differently, Plat5 picks a default and the deployment can change it. Plat5 fixes only what isolation, security, or the shared contract needs.

## Where each decision lives

| Question | Decided by |
|---|---|
| Who is this? | The IdP or identity, from the credential |
| Which node? | The path, resolved by the gateway from slugs or ids |
| May they call this route on this node? | The gateway: the route's permissions against the union of the member's bindings on the node and its ancestors |
| Does this member hold this permission on this node, outside a request or for someone other than the caller? | Identity, asked by the service, from the same bindings ([access](access.md#outside-a-request)) |
| Which nodes can they see? | Plat5, filtering node lists by each kind's `list_permission` |
| May they touch this record inside a node? | The service's state rules today; identity's evaluation API extended below nodes later ([`TODO.md`](../../TODO.md)) |
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

Data about the person themselves, the same in every tree (a profile, a setting), is keyed by the user id and served on user routes. Tree data is keyed by member ids, never a user id.

**Record the member id when logic depends on who did it; never decide access from it yourself.** The gateway decided for the caller; for anyone else, or outside a request, ask identity ([access](access.md#outside-a-request)). Separation of duties ("you can't approve your own change") is a service comparing two member ids on two records: a state rule, not an access check. Product features never read audit.

## Next

| To understand | Read |
|---|---|
| Who can do what, and how a request is decided | [`concepts/access.md`](access.md) |
| How nodes and members are created, deleted, restored and purged | [`concepts/lifecycle.md`](lifecycle.md) |
| How the deployment's config is applied | [`concepts/config.md`](config.md) |
| What gets audited | [`concepts/audit.md`](audit.md) |
| What Plat5 won't do, and how it can grow | [`concepts/boundaries.md`](boundaries.md) |
