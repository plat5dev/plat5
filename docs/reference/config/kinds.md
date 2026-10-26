# Kinds document

The tree's levels, in `plat5.yml`. Every node in identity is one of these kinds. Concept: [the model](../../concepts/model.md), [config](../../concepts/config.md).

```yaml
# in plat5.yml
kinds:
  organization:
    path: /orgs/{organization}
    list_permission: organization:read
  project:
    parent: organization
    path: /projects/{project}
    list_permission: project:read
```

<!-- generated from schema: kinds.json#/$defs/kind_name kinds.json#/$defs/kind/properties -->
| Field | Rule |
|-------|------|
| kind name | Lowercase identifier, `[a-z][a-z0-9_]*`, max 32. Not `public`, `member` or `user`. Names the kind in route scopes, path params, placeholders (`{project.id}`), headers (`X-Plat5-Project-Id`), events (`kind: project`) and audit. Fixed once nodes exist: a rename reads as a remove plus an add |
| `parent` | The one kind a node sits under. Omitted for a root |
| `path` | Required. The kind's piece of every public path: `/<collection>/{<kind name>}`, such as `/projects/{project}`. The collection has the slug shape. Plat5 joins the pieces root first, so a project's routes sit under `/orgs/{organization}/projects/{project}` |
| `list_permission` | Required. The permission that shows a node of this kind in a list, live or deleted ([lists](../../concepts/access.md#lists)) |
| `bind_creator` | Whether creating a node of this kind gives its creator `creator_role` on it ([assigning](../../concepts/access.md#assigning)). Default `true`. Off, the creator reaches the node through bindings above it; on a root kind, a new root has no member until a front door such as an operator plane adds one |
<!-- end generated -->

Each kind has one chain of kinds, so each node has one public path. Slugs are unique only within their parent, which is why the pieces nest in parent order. The kind's param, `{project}`, takes a slug or an id; it never reaches a service.

The other settings beside `kinds` in `plat5.yml`: [settings](settings.md). Slug shape, uniqueness and reuse are fixed rules, not config ([`concepts/model.md`](../../concepts/model.md#ids-and-slugs)).
