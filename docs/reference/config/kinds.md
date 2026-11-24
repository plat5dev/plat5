# Kinds document

The tree's levels, in the control-plane config. Every node in identity is one of these kinds. Concept: [the model](../../concepts/model.md), [config](../../concepts/config.md).

```yaml
kinds:
  organization:
    path: /orgs/{organization}
  project:
    parent: organization
    path: /projects/{project}
  environment:
    parent: project
    path: /envs/{environment}
    list_permission: project:read   # listed to whoever can read its project
```

<!-- generated from schema: kinds.json#/$defs/kind_name kinds.json#/$defs/kind/properties -->
| Field | Rule |
|-------|------|
| kind name | Lowercase identifier, `[a-z][a-z0-9_]*`, max 32. Not `public`, `member` or `user`. Names the kind in route scopes, path params, placeholders (`{project.id}`), headers (`X-Plat5-Project-Id`), events (`kind: project`) and audit. Fixed once nodes exist: a rename reads as a remove plus an add |
| `parent` | The one kind a node sits under. Omitted for a root |
| `path` | Required. The kind's piece of every public path: `/<collection>/{<kind name>}`, such as `/projects/{project}`. The collection has the slug shape. Plat5 joins the pieces root first, so a project's routes sit under `/orgs/{organization}/projects/{project}` |
| `list_permission` | The permission that shows a node of this kind in a list, live or deleted ([lists](../../concepts/access.md#lists)). Default: `<kind name>:read` |
| `bind_creator` | Whether creating a node of this kind gives its creator `creator_role` on it ([assigning](../../concepts/access.md#assigning)). Default `true`. Off, the creator reaches the node through bindings above it; on a root kind, a new root has no member until a front door such as an operator plane adds one |
| `host` | Optional, root kinds only. Hostname template resolving the leading subdomain label as this kind's slug or id: `{<kind name>}.<domain>`, such as `{organization}.api.example.com`. Requests to matching hosts resolve routes relative to the root alongside the path form |
| `allow_public` | Optional. Allows public principal bindings (`all_users`, `all_authenticated_users`) on nodes of this kind and `allow_public: true` routes in or below its scope. Default `false` |
<!-- end generated -->

Plat5 derives no permission names from a kind: routes name theirs literally. A kind reached through another kind's permissions says so in its routes and its `list_permission`, as `environment` does above with `project:*`.

Each kind has one chain of kinds, so each node has one public path. Slugs are unique only within their parent, which is why the pieces nest in parent order. The kind's param, `{project}`, takes a slug or an id; it never reaches a service.

Other top-level settings: [settings](settings.md). Slug shape, uniqueness and reuse are fixed rules, not config ([`concepts/model.md`](../../concepts/model.md#ids-and-slugs)).
