# Identity's and audit's resources

The routes identity and audit serve at the gateway. Plat5 fixes their operations and their upstream paths, since that's each service's own API. The deployment decides where each one sits publicly and what it requires, by overriding the defaults below. Nothing is generated: adding a kind gives it these routes. Concept: [access](../../concepts/access.md).

## The service files

`identity` and `audit` are reserved service names. Their files take `resources` instead of `scopes`, and hold only what differs from the defaults:

```yaml
# services/identity.yml
identity:
  url: http://identity:3000
  resources:                       # every kind
    node:
      methods:
        DELETE: { required_permissions: ["{kind}:admin"] }
    invites: false
  kinds:
    project:                       # one kind
      resources:
        children:
          path: /apps
```

```yaml
# services/audit.yml
audit:
  url: http://audit:3002
```

| Field | Rule |
|-------|------|
| `url`, `timeouts`, `max_body_bytes` | As for any [service](services.md#service-fields) |
| `resources` | Overrides for every kind, and for the user-scope resources |
| `kinds.<kind>.resources` | Overrides for one kind. They apply after `resources` |

Each key replaces its default one level down: a resource's `path`, or one verb under `methods`. `false` removes a resource, or a verb. `false` on a collection also removes the resources below it: `service_accounts` takes `service_account`, `keys` and `key`; `keys` takes `key`; `invites` takes `invite`; `bindings` takes `binding`; `members` takes `member`; `my_keys` takes `my_key`. An overridden path keeps the resource's params (`/grants/{binding_id}` for `binding`). Permissions can use `{kind}`: the [`permission_name`](kinds.md) of the kind the route is for, by default its name. A resource also takes the route-level `required_permissions`, `rate_limit` and `audit` of any [route](services.md#route-fields); `upstream` and `allow_deleted` are fixed.

A resource name or verb that isn't below, or a kind that isn't in `kinds`, is **422**. The expanded routes follow every route rule ([matching](services.md#matching) included), and their permissions count as required for roles. `plat5 routes list` prints them.

With audit off for the deployment, leave `services/audit.yml` out of `services`. A deployment that leaves `services/identity.yml` out has no identity routes at the gateway; other front doors can still call identity.

## Identity's defaults

For a kind `K` with parent `P`. Paths are relative to `K`'s node unless the scope says otherwise; `{kind}` is `K`'s `permission_name`, by default `K`. `<collection>` is the literal in `K`'s `path` (`/projects` for `/projects/{project}`).

| Resource | Scope | Path | Methods and permissions | Upstream on identity |
|---|---|---|---|---|
| `node` | `K` | `/` | `GET` `{kind}:read`; `PATCH` `{kind}:write`; `DELETE` `{kind}:delete` | `/nodes/{K.id}` |
| `restore` | `K` | `/restore` | `POST` `{kind}:delete`, with `allow_deleted` | `/nodes/{K.id}/restore` |
| `children` | `P`, for a `K` that isn't a root | `<collection>` | `GET` `any_member`, [filtered](../../concepts/access.md#lists); `POST` `{kind}:create` | `/nodes/{P.id}/children/K` |
| `membership` | `K` | `/membership` | `GET` `any_member`; on a root, `DELETE` `any_member` to leave the tree | `GET /nodes/{K.id}/memberships/{member.id}`; `DELETE /nodes/{K.id}/members/{member.id}` |
| `bindings` | `K` | `/bindings` | `GET` `{kind}:bindings:read`; `POST` `{kind}:bindings:write` | `/nodes/{K.id}/bindings` |
| `binding` | `K` | `/bindings/{binding_id}` | `PATCH`, `DELETE` `{kind}:bindings:write` | `/nodes/{K.id}/bindings/{binding_id}` |
| `service_accounts` | `K` | `/service-accounts` | `GET` `{kind}:bindings:read`; `POST` `{kind}:bindings:write` | `/nodes/{K.id}/service-accounts` |
| `service_account` | `K` | `/service-accounts/{service_account_id}` | `GET` `{kind}:bindings:read`; `PATCH`, `DELETE` `{kind}:bindings:write` | `/nodes/{K.id}/service-accounts/{service_account_id}` |
| `keys` | `K` | `/service-accounts/{service_account_id}/keys` | `GET`, `POST` `{kind}:bindings:write` | `/nodes/{K.id}/service-accounts/{service_account_id}/keys` |
| `key` | `K` | `/service-accounts/{service_account_id}/keys/{key_id}` | `DELETE` `{kind}:bindings:write` | `/nodes/{K.id}/service-accounts/{service_account_id}/keys/{key_id}` |
| `invites` | `K` | `/invites` | `GET`, `POST`: on a root `{kind}:bindings:write`, below one `guests:write` | `/nodes/{K.id}/invites` |
| `invite` | `K` | `/invites/{invite_id}` | `DELETE`, as `invites` | `/nodes/{K.id}/invites/{invite_id}` |
| `roles` | `K` | `/roles` | `GET` `any_member` | `/nodes/{K.id}/roles` |
| `members` | `K`, a root | `/members` | `GET` `{kind}:bindings:read`; `POST` `{kind}:bindings:write` | `/nodes/{K.id}/members` |
| `member` | `K`, a root | `/members/{member_id}` | `GET` `any_member`; `DELETE` `{kind}:bindings:write` | `/nodes/{K.id}/members/{member_id}` |
| `roots` | `user`, for a root `K` | `<collection>` | `POST` | `/roots/K` |
| `me` | `user` | `/user` | `GET`, `DELETE` | `/users/{user.id}` |
| `my_nodes` | `user` | `/user/nodes` | `GET` | `/users/{user.id}/nodes` |
| `my_keys` | `user` | `/user/keys` | `GET`, `POST` | `/users/{user.id}/keys` |
| `my_key` | `user` | `/user/keys/{key_id}` | `DELETE` | `/users/{user.id}/keys/{key_id}` |
| `redeem` | `user` | `/user/invites/redeem` | `POST` | `/users/{user.id}/invites/redeem` |

`roots` is set per root kind (`kinds.organization.resources.roots`); the other user-scope resources only under `resources`. What each upstream does: [identity's API](../api/identity.md).

Shown for `organization` (`/orgs/{organization}`) and `project` under it (`/projects/{project}`):

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

## Audit's defaults

| Resource | Scope | Path | Methods and permissions | Upstream on audit |
|---|---|---|---|---|
| `events` | `K` | `/audit-events` | `GET` `audit:read` | `/nodes/{K.id}/audit-events` |
| `my_events` | `user` | `/user/audit-events` | `GET` | `/users/{user.id}/audit-events` |

A node's events include everything below it ([`reference/api/audit.md`](../api/audit.md#reading)).

## The default permissions

`<kind>` is the kind's `permission_name`, by default its name.

| Permission | Checked at | Allows |
|------------|------------|--------|
| `<kind>:read` | The node | Getting the node; the default `list_permission` |
| `<kind>:create` | The parent | Creating one under the parent |
| `<kind>:write` | The node | Changing the display name |
| `<kind>:delete` | The node | Deleting and restoring |
| `<kind>:bindings:read` / `:write` | The node | Listing or changing who has access here, service accounts and their keys included. On a root, also adding members and invites to join |
| `guests:write` | The invite's node or an ancestor | Invites below the root, which can admit people outside the tree. Granted to no role (counting `*`), guests are off. A role bindable below the root that grants it, or `*`, lets its holders invite guests there |
| `audit:read` | The node or an ancestor | Reading audit for the node and below |

**These are defaults, not rules.** The deployment can rename them, change them or remove routes. Identity's code depends on no permission name: lists use each kind's `list_permission` ([lists](../../concepts/access.md#lists)). Roles grant them like any permission; `*` grants all.

Whoever holds `<kind>:bindings:write` can create a service account with any role bindable there, and bind themselves any role: it's as strong as the strongest role ([who may assign](../../concepts/access.md#who-may-assign)).
