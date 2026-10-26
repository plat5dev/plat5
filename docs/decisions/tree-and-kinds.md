# Tree and kinds

Why Plat5 owns a tree whose kinds the deployment declares, and why nodes, ids, slugs and paths are shaped as they are. The rules: [`concepts/model.md`](../concepts/model.md), [`reference/config/kinds.md`](../reference/config/kinds.md), [`reference/config/resources.md`](../reference/config/resources.md).

## A tree of kinds from config

Every tenant scope is built from config, so no kind is special: organizations and projects are both kinds. Plat5 owns the tree (ids, parents, who can reach each node); services own everything inside a node. A product needing projects inside orgs, with keys, roles, rate limits and audit scoped to a project, gets them without each service inventing its own access model.

| Decision | Why |
|----------|-----|
| Kinds are declared in config; no kind is special to Plat5 code | A fixed tree with configurable names is simpler to build, but gives up "no scope is special": Plat5's code would know the levels, and a product with other levels would bend to them |
| Kinds are only the tenant tree. A person's own routes are a built-in `user` scope beside `public`, not a kind | A sign-in kind would be one node with no slug, bindings, permissions or children, and every rule would carry an exception for it. A person isn't inside a tree; they're bound into trees |
| A node holds `id`, `kind`, `parent_id`, `root_id`, `slug`, `display_name`, `status`, nothing else | Everything else about a node is service data keyed by its id, so the node never grows into a product's data model |
| A thing belongs in the tree if a key, a role, a rate limit or an audit filter would ever attach to it | Projects pass. Plans, features and subscribers don't: they're service data keyed by node id |
| A kind carries only what something reads: `parent`, `path` and `list_permission`. It has no display name | A console knows its own kinds. Readable and reviewable in a diff, with no hidden behaviour |
| A kind has one parent | Several parents would only make a kind optional in the middle, and cost every route one copy per chain. With one chain per kind, a route has one public path and an upstream can use any ancestor's id. A list of parents can come back later without breaking anything |
| Kind names can't be `public`, `member` or `user` | `public` and `user` are scopes; `member` and `user` would clash with the `{member.id}` and `{user.id}` placeholders |
| `display_name` and `name_claim` are at most 128 characters | Same limit as other names |

## Ids, slugs and display names

A node has a generated id, a slug fixed at creation, and a display name that can change: the id for storage, the slug for paths and for configs that name a node, the display name for people.

| Decision | Why |
|----------|-----|
| Ids are bare lowercase ULIDs, with no type prefix, in one form only | One spelling per id: nothing to normalize, nothing to compare twice. The type is known from where the id appears |
| Secrets keep a prefix: `{brand}-k-2-…` keys and `inv_…` invite tokens | A fixed prefix makes a secret recognizable wherever it leaks: secret scanners and log filters match on it, and a person tells a key from an invite at a glance |
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

Identity's and audit's routes come from resources with documented defaults ([resources](../reference/config/resources.md)); the deployment overrides them and nothing is generated.

| Decision | Why |
|----------|-----|
| Plat5 fixes identity's operations and their upstream paths; the deployment decides where each one sits publicly and what it requires | Identity's API is identity's own, and the public shape and permissions are the deployment's. Adding a kind gives it an API with nothing to regenerate or merge by hand |
| `services/identity.yml` and `services/audit.yml` hold only overrides, for every kind or for one; each key replaces its default one level down, and `false` removes a resource or a verb. `false` on a collection also removes the resources below it | A deployment writes only what differs, so the defaults can improve without a merge. An item route without its collection has no use, and per-verb `false` keeps the item while turning off a collection's create |
| `identity` and `audit` are reserved service names | Plat5 knows their API; any other name is an ordinary service |
| Identity's own API addresses everything by id: `/nodes/{id}/…`, `/roots/{kind}`, `/users/{id}/…`. A service account is addressed by its member id | Any front door can call it without slugs. Slugs exist only at the gateway |
| `plat5 routes list` prints every service's expanded routes | Defaults are only safe to rely on when the result is easy to read |
