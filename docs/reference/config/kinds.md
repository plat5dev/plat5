# Kinds document

The tree's levels. Concept: [the model](../../concepts/model.md), [config](../../concepts/config.md).

The tree's levels. Every node in identity is one of these kinds.

```yaml
version: 2
deletion_grace: 30d
reserved_slugs: [new, settings, api, admin, me]
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

| Field | Rule |
|-------|------|
| kind name | Lowercase identifier, `[a-z][a-z0-9_]*`, max 32. Not `public`. Names the kind in route scopes, path params, subject fields (`{subject.project_id}`), headers (`X-Plat5-Project-Id`), events (`project.deleted`) and audit. Fixed once nodes exist: a rename reads as a remove plus an add |
| `parent` | The kind, or list of kinds, a node sits under. Omitted for a root |
| `label` | What consoles show. Changes any time |
| `sign_in` | `idp`. Marks the one kind people sign in to: a single-node root (the directory), created at boot. Exactly one kind has it |
| `name_claim` | On the sign-in kind only. The IdP claim holding a person's name. Default `name` |
| `segment` | The path segment for generated routes. Default: the kind name plus `s` |
| `reserved_slugs` | Slugs no node may take, deployment-wide |
| `deletion_grace` | How long a deleted node stays restorable before it's purged. One value for every kind. Default `30d` |

Slug shape, uniqueness and reuse are fixed rules, not config ([`concepts/model.md`](../../concepts/model.md#ids-and-slugs)).
