# Model

> The concepts and principles everything else builds on. When another doc disagrees with this page, the other doc is wrong. Why each decision was made: [the decision records](../decisions/README.md).

Plat5 is the front door for multi-tenant APIs. It owns a tree of nodes whose kinds the deployment declares, the principals who can reach each node, and the check at the gateway. Services own everything inside a node.

On every request the gateway answers three questions: who is calling (the credential), which node the request is about (the path), and whether the caller's bindings on that node or its ancestors grant a label the route requires. It then forwards the request with node ids and the caller's member id. Slugs and person ids never reach a service.

## Concepts

| Concept | What it is |
|---|---|
| **Kind** | A level of the tree, declared in the `kinds` document: `directory`, `organization`, `project`, … No kind is special to Plat5 code ([`reference/config/kinds.md`](../reference/config/kinds.md)) |
| **Node** | One instance of a kind: `id`, `kind`, `parent`, `root`, `slug`, `label`, `status`. Nothing else. Everything else about a node is service data keyed by its id |
| **Root** | A node with no parent. Each root and the nodes under it are one **tree**: the isolation boundary. Organizations are roots |
| **Sign-in node** | The one root of the kind marked `sign_in` (the directory). Every person has a member there. It has no slug and no bindings |
| **Principal** | A person (born at first sign-in) or a service account (born at a node of an org's tree). Its id never leaves Plat5 |
| **Member** | A principal's presence in one tree: one per principal per tree, kept after removal. The id services see as the caller (`X-Plat5-Member-Id`). In the directory it's the person's user id |
| **Binding** | A role for a member on one node of the member's tree, optionally expiring. A member can have many |
| **Guest** | A member with no binding on its tree's root, only on nodes below it. Created by redeeming an invite below the root. Derived, not stored |
| **Role** | The deployment's name for a set of labels, bindable at some kinds ([`concepts/access.md`](access.md)). Plat5 names no roles |
| **Label** | A permission a route can require. Only the gateway checks labels |
| **Route scope** | `scope: <kind>` addresses a node of that kind; `scope: public` has no subject ([`reference/config/routes.md`](../reference/config/routes.md)) |
| **Subject** | What the gateway sends a service: each node's id in the chain, and the caller's member id in that tree |
| **Credential** | An IdP token or a personal API key (people), or a service account key. Belongs to the principal, not to a tree |
| **Front door** | Anything on the private network that calls services. Plat5's gateway is one; an operator plane is another |
| **Audit log** | Per root. Who did what, for reporting and compliance ([`concepts/audit.md`](audit.md)) |
| **Lifecycle event** | A birth, delete, restore or purge of a node, or a member joining or leaving a tree ([`concepts/lifecycle.md`](lifecycle.md#lifecycle-events)) |

## Ids and slugs

Ids are ULIDs with a type prefix: `n_` nodes, `m_` members, `b_` bindings, `k_` keys, `i_` invites. A principal's id never leaves identity.

Every node except the sign-in node has a slug, fixed at creation. Fixed rules, not config:

| Rule | |
|------|--|
| Shape | Lowercase letters, digits and hyphens; starts with a letter, doesn't end with a hyphen; 3 to 63 characters. Never an id: ids contain `_` |
| Uniqueness | Per parent and kind. Roots share one namespace, so org slugs are deployment-wide |
| Reuse | Never, within the same parent. A purged node keeps its row as a tombstone (slug, id, kind, parent). A slug can be released by hand ([slug release](../reference/api/identity.md#slug-release)) |
| Generation | Omitted on create: generated from the label, with `-2`, `-3` on collision |
| Reserved | `reserved_slugs` in the `kinds` document. **422** |

A path segment that contains `_` is read as a node id, anything else as a slug. Both resolve under the previous node.

## Principles

1. **The tree is config; nodes are data; no scope is special.**
2. **Plat5 owns the tree and who can reach each node; services own what's inside a node.**
3. **Access is decided only at the gateway,** from the union of a member's bindings on the node and its ancestors.
4. **The path names the node; the credential names the principal.**
5. **Services see node ids and the caller's member id, never a person's id.**
6. **Services do no auth, so any front door can call them.** A service's API is its paths and documented headers, reachable only on the private network. Plat5's gateway is one front door; others (an operator plane, an internal job, a custom gateway) bring their own rules and docs.
7. **The deployment's data, not Plat5's names.** Kinds, roles, labels and routes are all config.
8. **Dangerous changes are allowed once planned and confirmed; deletes are soft.**

## Where each decision lives

| Question | Decided by |
|---|---|
| Who is this? | The IdP or identity, from the credential |
| Which node? | The path, resolved by the gateway from slugs or ids |
| May they call this route on this node? | The gateway: the route's labels against the union of the member's bindings on the node and its ancestors |
| Which nodes can they see? | Plat5, filtering node lists by the list route's labels |
| May they touch this record inside a node? | The service's state rules today; the deferred policy hook later ([`TODO.md`](../../TODO.md)) |
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

## Next

| To understand | Read |
|---|---|
| Who can do what, and how a request is decided | [`concepts/access.md`](access.md) |
| How nodes and members are born, deleted, restored and purged | [`concepts/lifecycle.md`](lifecycle.md) |
| How the deployment's config is applied | [`concepts/config.md`](config.md) |
| What gets audited | [`concepts/audit.md`](audit.md) |
| What Plat5 won't do, and how it can grow | [`concepts/boundaries.md`](boundaries.md) |
