# Generated routes

The routes `plat5 routes generate` writes into `services/identity.yml` and `services/audit.yml`: identity's and audit's public routes, for every kind in `plat5.yml`. What the generator reads and how it names permissions: [CLI](../cli.md#generating-identitys-and-audits-routes). Concept: [access](../../concepts/access.md).

The two files are ordinary [service files](services.md). Once written, they are the truth: the control plane serves what they say, and nothing is added to them at apply. Generating is scaffolding: drop a route by deleting it, move one by editing its `path`, change what one requires by editing its permissions. Regenerating overwrites both files ([when to](../cli.md#generating-identitys-and-audits-routes)).

## The files

One scope block per kind, with `upstream_prefix: /nodes/{K.id}`, so most routes need no `upstream`. A route whose upstream isn't the prefix plus its path names it. Each verb carries its own `required_permissions`. Shown for `organization` (`/orgs/{organization}`) and `project` under it (`/projects/{project}`), cut short:

```yaml
# services/identity.yml
identity:
  url: http://identity:3000
  scopes:
    organization:
      upstream_prefix: /nodes/{organization.id}
      routes:
        - path: /
          methods:
            GET: { required_permissions: [organization:read] }
            PATCH: { required_permissions: [organization:write] }
            DELETE: { required_permissions: [organization:delete] }
        - path: /restore
          allow_deleted: true
          methods:
            POST: { required_permissions: [organization:delete] }
        - path: /projects
          upstream: /nodes/{organization.id}/children/project
          methods:
            GET: { required_permissions: any_member }
            POST: { required_permissions: [project:create] }
        - path: /membership
          upstream: /nodes/{organization.id}/memberships/{member.id}
          methods:
            GET: { required_permissions: any_member }
            DELETE: { required_permissions: any_member }
        # …
    user:
      routes:
        - path: /orgs
          upstream: /roots/organization
          methods: [POST]
        - path: /user
          upstream: /users/{user.id}
          methods: [GET, PATCH, DELETE]
        # …
```

```yaml
# services/audit.yml
audit:
  url: http://audit:3002
  scopes:
    organization:
      upstream_prefix: /nodes/{organization.id}
      routes:
        - path: /audit-events
          methods:
            GET: { required_permissions: [audit:read] }
    # … one block per kind
    user:
      routes:
        - path: /user/audit-events
          upstream: /users/{user.id}/audit-events
          methods: [GET]
```

The generator sets no `rate_limit` or `audit`, so the [defaults](services.md#route-fields) apply.

## Identity's routes

For a kind `K` with parent `P`. `<p>` is `K`'s permission prefix ([CLI](../cli.md#generating-identitys-and-audits-routes)): `K`'s name, unless its `list_permission` names another. `<collection>` is the literal in `K`'s `path` (`/projects` for `/projects/{project}`). Paths are relative to the scope's node.

| Scope | Path | Methods and permissions | Upstream on identity |
|---|---|---|---|
| `K` | `/` | `GET` `<p>:read`; `PATCH` `<p>:write`; `DELETE` `<p>:delete` | `/nodes/{K.id}` |
| `K` | `/restore` | `POST` `<p>:delete`, with `allow_deleted: true` | `/nodes/{K.id}/restore` |
| `P`, for a `K` that isn't a root | `<collection>` | `GET` `any_member`, [filtered](../../concepts/access.md#lists); `POST` `<p>:create` | `/nodes/{P.id}/children/K` |
| `K` | `/membership` | `GET` `any_member`; on a root, `DELETE` `any_member` to leave the tree | `/nodes/{K.id}/memberships/{member.id}` |
| `K` | `/bindings` | `GET` `<p>:bindings:read`; `POST` `<p>:bindings:write` | `/nodes/{K.id}/bindings` |
| `K` | `/bindings/{binding_id}` | `PATCH`, `DELETE` `<p>:bindings:write` | `/nodes/{K.id}/bindings/{binding_id}` |
| `K` | `/service-accounts` | `GET` `<p>:bindings:read`; `POST` `<p>:bindings:write` | `/nodes/{K.id}/service-accounts` |
| `K` | `/service-accounts/{service_account_id}` | `GET` `<p>:bindings:read`; `PATCH`, `DELETE` `<p>:bindings:write` | `/nodes/{K.id}/service-accounts/{service_account_id}` |
| `K` | `/service-accounts/{service_account_id}/keys` | `GET`, `POST` `<p>:bindings:write` | `/nodes/{K.id}/service-accounts/{service_account_id}/keys` |
| `K` | `/service-accounts/{service_account_id}/keys/{key_id}` | `DELETE` `<p>:bindings:write` | `/nodes/{K.id}/service-accounts/{service_account_id}/keys/{key_id}` |
| `K` | `/invites` | `GET`, `POST`: on a root `<p>:bindings:write`, below one `guests:write` | `/nodes/{K.id}/invites` |
| `K` | `/invites/{invite_id}` | `DELETE`, as `/invites` | `/nodes/{K.id}/invites/{invite_id}` |
| `K` | `/roles` | `GET` `any_member` | `/nodes/{K.id}/roles` |
| `K`, a root | `/members` | `GET` `<p>:bindings:read` | `/nodes/{K.id}/members` |
| `K`, a root | `/members/{member_id}` | `GET` `any_member`; `DELETE` `<p>:bindings:write` | `/nodes/{K.id}/members/{member_id}` |
| `user`, for a root `K` | `<collection>` | `POST` | `/roots/K` |
| `user` | `/user` | `GET`, `PATCH`, `DELETE` | `/users/{user.id}` |
| `user` | `/user/nodes` | `GET` | `/users/{user.id}/nodes` |
| `user` | `/user/keys` | `GET`, `POST` | `/users/{user.id}/keys` |
| `user` | `/user/keys/{key_id}` | `DELETE` | `/users/{user.id}/keys/{key_id}` |
| `user` | `/user/invites/redeem` | `POST` | `/users/{user.id}/invites/redeem` |

Adding a member to a root by IdP subject (`POST /nodes/{root_id}/members`) isn't generated: it trusts the caller to name a person by raw IdP subject, which suits an operator plane or provisioning more than a customer. A deployment that wants it at the gateway adds the route to `/members`, with the permission it chooses. What each upstream does: [identity's API](../api/identity.md).

Shown for `organization` and `project`:

| Public route | Permissions |
|---|---|
| `POST /orgs` | user route |
| `GET`, `PATCH`, `DELETE /orgs/{organization}` | `organization:read`, `:write`, `:delete` |
| `GET /orgs/{organization}/projects` | `any_member`, filtered by `project:read` |
| `POST /orgs/{organization}/projects` | `project:create` |
| `GET /orgs/{organization}/projects/{project}` | `project:read` |
| `POST /orgs/{organization}/projects/{project}/restore` | `project:delete` |
| `GET /orgs/{organization}/projects/{project}/membership` | `any_member` |
| `DELETE /orgs/{organization}/membership` | `any_member`: leave the org |
| `GET, POST /orgs/{organization}/projects/{project}/invites` | `guests:write` |
| `GET, POST /orgs/{organization}/invites` | `organization:bindings:write` |
| `GET /user/nodes?kind=organization` | user route: the org switcher |

## Audit's routes

| Scope | Path | Methods and permissions | Upstream on audit |
|---|---|---|---|
| `K` | `/audit-events` | `GET` `audit:read` | `/nodes/{K.id}/audit-events` |
| `user` | `/user/audit-events` | `GET` | `/users/{user.id}/audit-events` |

A node's events include everything below it ([`reference/api/audit.md`](../api/audit.md#reading)).

## The generated permissions

| Permission | Checked at | Allows |
|------------|------------|--------|
| `<p>:read` | The node | Getting the node; also the kind's `list_permission` by default |
| `<p>:create` | The parent | Creating one under the parent |
| `<p>:write` | The node | Changing the display name |
| `<p>:delete` | The node | Deleting and restoring |
| `<p>:bindings:read` / `:write` | The node | Listing or changing who has access here, service accounts and their keys included. On a root, also listing and removing members, and invites to join |
| `guests:write` | The invite's node or an ancestor | Invites below the root, which can admit people outside the tree. Granted to no role (counting `*`), guests are off. A role bindable below the root that grants it, or `*`, lets its holders invite guests there |
| `audit:read` | The node or an ancestor | Reading audit for the node and below |

**These are a starting point, not rules.** The files can rename them, change them or drop routes. Identity's code depends on no permission name: lists use each kind's `list_permission` ([lists](../../concepts/access.md#lists)). Roles grant them like any permission; `*` grants all.

Whoever holds `<p>:bindings:write` can create a service account with any role bindable there, and bind themselves any role: it's as strong as the strongest role ([who may assign](../../concepts/access.md#who-may-assign)).
