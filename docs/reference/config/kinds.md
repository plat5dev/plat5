# Kinds document

The tree's levels, in `plat5.yml`. Every node in identity is one of these kinds. Concept: [the model](../../concepts/model.md), [config](../../concepts/config.md).

```yaml
# in plat5.yml
deletion_grace: 30d
reserved_slugs: [new, settings, api, admin]
kinds:
  directory:
    sign_in: idp
    name_claim: name
  organization:
    label: Organization
    segment: orgs
  project:
    label: Project
    parent: organization
```

<!-- generated from schema: kinds.json#/$defs/kind_name kinds.json#/$defs/kind/properties kinds.json#/properties/reserved_slugs kinds.json#/properties/deletion_grace -->
| Field | Rule |
|-------|------|
| kind name | Lowercase identifier, `[a-z][a-z0-9_]*`, max 32. Not `public` or `member`. Names the kind in route scopes, path params, subject fields (`{subject.project_id}`), headers (`X-Plat5-Project-Id`), events (`project.deleted`) and audit. Fixed once nodes exist: a rename reads as a remove plus an add |
| `parent` | The kind, or list of kinds, a node sits under. Omitted for a root |
| `label` | What consoles show. Changes any time |
| `sign_in` | `idp`. Marks the one kind people sign in to: a single-node root (the directory), created at boot. Exactly one kind has it |
| `name_claim` | On the sign-in kind only. The IdP claim holding a person's name. Default `name` |
| `segment` | The path segment for generated routes. Slug shape: lowercase letters, digits and hyphens, starting with a letter, max 63. Default: the kind name with `_` as `-`, plus `s` (`service_area` → `service-areas`) |
| `reserved_slugs` | Slugs no node may take, deployment-wide |
| `deletion_grace` | How long a deleted node stays restorable before it's purged. One value for every kind. A duration: a whole number and `s`, `m`, `h` or `d` (`30d`, `12h`). Default `30d`. A change applies to new deletes only |
<!-- end generated -->

Slug shape, uniqueness and reuse are fixed rules, not config ([`concepts/model.md`](../../concepts/model.md#ids-and-slugs)).
