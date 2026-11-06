# Tree and kinds

Why Plat5 owns a tree whose kinds the deployment declares, and why nodes, ids, slugs and paths are shaped as they are. The rules: [`concepts/model.md`](../concepts/model.md), [`reference/config/kinds.md`](../reference/config/kinds.md), [`reference/config/generated-routes.md`](../reference/config/generated-routes.md).

## A tree of kinds from config

Every tenant scope is built from config, so no kind is special: organizations and projects are both kinds. Plat5 owns the tree (ids, parents, who can reach each node); services own everything inside a node. A product needing projects inside orgs, with keys, roles, rate limits and audit scoped to a project, gets them without each service inventing its own access model.

| Decision | Why |
|----------|-----|
| Kinds are declared in config; no kind is special to Plat5 code | A fixed tree with configurable names is simpler to build, but gives up "no scope is special": Plat5's code would know the levels, and a product with other levels would bend to them |
| Kinds are only the tenant tree. A person's own routes are a built-in `user` scope beside `public`, not a kind | A sign-in kind would be one node with no slug, bindings, permissions or children, and every rule would carry an exception for it. A person isn't inside a tree; they're bound into trees |
| A node holds `id`, `kind`, `parent_id`, `root_id`, `slug`, `display_name`, `status`, nothing else | Everything else about a node is service data keyed by its id, so the node never grows into a product's data model |
| A thing belongs in the tree if a key, a role, a rate limit or an audit filter would ever attach to it | Projects pass. Plans, features and subscribers don't: they're service data keyed by node id |
| A kind carries only what something reads: `parent`, `path`, `list_permission` and `bind_creator`. It has no display name | A console knows its own kinds. Readable and reviewable in a diff, with no hidden behaviour |
| A kind has no name in permissions; routes name theirs literally, and kinds can share permissions | Plat5 has no permission inheritance; bindings flow down the chain, and permissions are strings. A kind that's part of its parent, like an environment in a project, needs no permissions of its own: its routes require `project:*` and its `list_permission` is `project:read`, in plain sight where they're read |
| A kind has one parent | Several parents would only make a kind optional in the middle, and cost every route one copy per chain. With one chain per kind, a route has one public path and an upstream can use any ancestor's id. A list of parents can come back later without breaking anything |
| Kind names can't be `public`, `member` or `user` | `public` and `user` are scopes; `member` and `user` would clash with the `{member.id}` and `{user.id}` placeholders |
| `display_name`, a person's name, and each claim path in `users.name.claims` are at most 128 characters | Same limit as other names |

## Ids, slugs and display names

A node has a generated id, a slug fixed at creation, and a display name that can change: the id for storage, the slug for paths and for configs that name a node, the display name for people.

| Decision | Why |
|----------|-----|
| Ids are bare lowercase ULIDs, with no type prefix, in one form only | One spelling per id: nothing to normalize, nothing to compare twice. The type is known from where the id appears |
| Secrets keep a prefix, in one format: `{brand}-k-2-…` keys and `{brand}-i-1-…` invite tokens | A fixed prefix makes a secret recognizable wherever it leaks: secret scanners and log filters match on it, and a person tells a key from an invite at a glance. A deployment's brand is on every secret it hands out, not only on keys |
| Every node has a slug; slugs route | Machines put slugs in a config file once and they never change; people get readable console URLs; services store ids only, with slug handling once, in Plat5 |
| Slug shape: lowercase letters, digits and hyphens, starting with a letter, not ending with a hyphen, 3 to 63 characters | Safe in a URL and a DNS label with no escaping, and 63 characters is a DNS label's limit. Lowercase only, so two slugs never differ by case alone. A slug starts with a letter and an id with a digit, so a path segment is never ambiguous |
| Slugs are unique per parent and kind; roots share one namespace | A person's credential reaches every org they're in, so org slugs must be deployment-wide; project slugs only need to be unique in their org |
| A slug below a root is free again once its node is purged. A root's slug is tombstoned for good; an operator can release one | Inside one org, the people reusing a name are the org's own. Org slugs are deployment-wide and sit in other people's configs, so a machine holding an old org slug must never reach a stranger's org |
| A slug omitted on create is generated from the display name: lowercased, other characters to `-`, trimmed, filled or prefixed to fit the shape, `-2`, `-3` on collision | Readable, and always valid |

## Paths name the chain

| Decision | Why |
|----------|-----|
| Each kind declares its piece of the path (`/projects/{project}`), and Plat5 joins the pieces root first. Routes are written relative to their node | The public URL shape is set once, in `plat5.yml`; service files hold only what's the service's own. A path that names the chain says where a request lands just by reading it |
| The pieces nest in parent order | Slugs are unique only within their parent, so the parent has to be in the path. A flat, id-only path can come later |
| Node ids are accepted wherever a slug is | A client holding an id never needs the slug |
| The gateway resolves slugs to ids and forwards only ids, with every ancestor's id available to the upstream | Services store ids only |

## Identity's and audit's routes

Identity's and audit's public routes are in their service files, written by the CLI's generator and then the deployment's ([generated routes](../reference/config/generated-routes.md)). Why the files are explicit: [config and apply](config-and-apply.md#identitys-and-audits-routes).

| Decision | Why |
|----------|-----|
| Identity fixes its operations and their upstream paths; the deployment decides where each one sits publicly and what it requires | Identity's API is identity's own, and the public shape and permissions are the deployment's |
| No service name is reserved. The gateway reaches identity's admit and audit's writer through its env, not through a service file | Plat5's code needs no service file to be identity's or audit's, so their files are read like any other |
| Identity's own API addresses everything by id: `/nodes/{id}/…`, `/roots/{kind}`, `/users/{id}/…`. A service account is addressed by its member id | Any front door can call it without slugs. Slugs exist only at the gateway |

## Identity's errors

The rule: [`reference/errors.md`](../reference/errors.md#which-status).

| Decision | Why |
|----------|-----|
| **409** when the current state refuses a request, **422** when its input is invalid | A client can tell "fix what you sent" from "it can't be done now" by status alone |
| `CONFLICT` only for something that already exists; each other state refusal has its own code (`PARENT_DELETED`, `GRACE_EXPIRED`, `NODE_PURGED`, `SLUG_NOT_RELEASABLE`, `INVITE_NOT_REDEEMABLE`, `NAME_OWNED_BY_IDP`) | Clients branch on `code` and status, never on `details`. One code for six refusals made them parse `details.field` |
| An id that names nothing the request can use, or the wrong kind of thing, is **422**: a binding for a member not active in the tree, a service account's member on the members route, a child list of a kind that can't go there | No change of state makes the same request right: a member id belongs to one tree, removal is final, and a service account's member is never a person's. A list of a kind that can't go there is the same mistake as creating one, so it's the same **422**, not an empty list |
| Postgres unavailable is **503** `SERVICE_UNAVAILABLE` in every service, identity's lifecycle log and evaluation included | **503** says "retry later", which is what a caller should do; a **500** says there's a bug |
