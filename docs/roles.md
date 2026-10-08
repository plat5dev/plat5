# Roles

A deployment's roles. Plat5 ships no role names. The deployment's roles file names each role and says which labels it grants.

Boundary: [`identity-boundary.md`](identity-boundary.md). Identity API: [`identity.md`](identity.md). Route labels: [`routes.md`](routes.md).

## The model

Two things decide what a member credential may call.

| Piece | Says | Owner |
|-------|------|-------|
| Route `required_labels` | Which labels the route needs (any one of them) | The service's `routes.yml` |
| Role | Which labels the member holds | The deployment's roles file, resolved by identity |

Roles are on or off for the whole deployment. There is no mix.

| | Roles on (`ROLES_FILE` set) | Roles off (`ROLES_FILE` unset) |
|---|---|---|
| Member `role` | A slug from the file. Writes always set one | Always `null` |
| Validate and session mint `labels` | The role's list as the file writes it | `["*"]` |
| Route `required_labels` | Checked | Never blocks a member |
| `role` in a request body | A slug in the file, or **422** | **422** |
| [Last creator role](#last-creator-role) | Enforced | Nothing to keep |
| `GET /organizations/{organization_id}/roles` | The file | `roles: []`, the three role fields `null` |

Roles off is for a deployment where something else decides authorization: a policy engine, or the services themselves. Every member then holds every label, so route labels, including the identity catalog's, stop no one.

A credential belongs to a principal and carries that principal's permissions. A member key or member session carries the member's role labels. Nothing narrows it: keys and sessions have no labels of their own. To give a machine less, give it a service account with a smaller role.

The gateway checks the member's labels against the route. It does not see roles. Services see neither: the gateway is the only place this level of access is decided.

User credentials on `user` routes have no role. Roles are org membership.

Resource authorization (this project, this document) is not a role. It stays in the service, or in a policy engine the service calls.

## File

`ROLES_FILE` on identity is the path to a YAML file. Identity reads it at boot. Unset or empty turns roles off, and identity logs a warning. A file that fails validation refuses boot. To change roles, edit the file and restart identity.

```yaml
roles:
  owner: ["*"]
  admin: [org:write, org:members:write, org:service-accounts:write, org:audit:read]
  member: []
creator_role: owner
default_role: member
```

| Field | Rule |
|-------|------|
| `roles` | Map of slug → label list. At least one role. |
| slug | `[a-z0-9:._-]+`, max 64. Opaque to Plat5. |
| label list | Same hygiene as route `required_labels`: `[a-z0-9:._-]+`, max 64 chars, unique. Max 64 labels. `[]` grants no labels. `["*"]` grants every label; `*` is valid only alone. |
| `creator_role` | Required. A slug in `roles`. Assigned to whoever creates an org. |
| `default_role` | Required. A slug in `roles`. Assigned when a write omits `role`. |
| `service_account_default_role` | Optional. A slug in `roles`. Assigned when a service-account create omits `role`. Unset → `default_role`. |

Unknown keys refuse boot.

`plat5 init` writes the file above as a starter, and [`compose/roles.yml`](../compose/roles.yml) is the same file. Templates ship their own, with their app's labels. None of them is Plat5's. Edit them.

## Resolution

Identity resolves the role at member key validate and member session validate. The validate `labels` is the role's labels. The gateway never gets the role, only its labels.

| Member role | Validate `labels` |
|-------------|-------------------|
| `["*"]` | `["*"]` |
| a label list | that list |
| a slug not in the file | `[]` |
| any, roles off | `["*"]` |

`labels` is always a list, and it says what the file says. `*` is every label: the gateway matches it to any route label. A route cannot require `*` itself.

A slug removed from the file grants nothing, so members who still hold it fail closed.

The gateway caches validate for `APIKEY_CACHE_TTL_SECS`. A role change is visible at the edge when that TTL expires, like a suspend. It applies to existing keys and sessions, because validate resolves the role every time. A key follows its member's role: it gains labels the role gains, and loses labels the role loses.

## Assigning

| Write | Role |
|-------|------|
| `POST /users/{user_id}/organizations` | `creator_role`. No body field. |
| `POST /organizations/{organization_id}/members` | `role` in the body, or `default_role` |
| `POST /organizations/{organization_id}/invites` | `role` in the body, or `default_role`. Stored on the invite. Redeem assigns it. |
| `POST /organizations/{organization_id}/service-accounts` | `role` in the body, or `service_account_default_role` (unset → `default_role`) |
| `PATCH /organizations/{organization_id}/members/{member_id}` | `role` in the body |

A slug not in the file is **422**. With roles off, every write above leaves `role` empty, and `role` in a body is **422**.

## Who may assign

There is no grant cap. Identity does not compare the caller to the role it assigns, or to the member it acts on. Whoever may call the route may assign any role, including one with more labels than its own, and may act on any member. The route's labels decide who may call. That is the gateway's check ([`routes.md`](routes.md)).

A service account is the org's, not its creator's. Its role is its own, and its keys carry that role. Whoever may create a service account, or mint its keys, may create one with any role.

So the labels that reach these routes are as strong as the strongest role. In the starter roles file, `admin` holds `org:members:write`, so an `admin` can make any member, itself included, an `owner`. Give those labels only to members you would trust with every role.

## Last creator role

A write may not take an org's count of non-removed members holding `creator_role` from one to zero. With roles on, every member holds a role, so every member counts. That covers a demotion, a remove at either address, and a service-account delete. **422**. Suspending is allowed, as it is for the last member. Deleting the org is not blocked. With roles off there is no `creator_role`, and only the last-member rule applies.

## Listing roles

`GET /organizations/{organization_id}/roles` returns the deployment's roles. The catalog publishes it as `GET /org/roles`, unlabeled, so a console can show a role picker.

```json
{
  "roles": [
    { "slug": "admin", "labels": ["org:write", "org:members:write", "org:service-accounts:write", "org:audit:read"] },
    { "slug": "member", "labels": [] },
    { "slug": "owner", "labels": ["*"] }
  ],
  "creator_role": "owner",
  "default_role": "member",
  "service_account_default_role": "member"
}
```

`labels` is the file's list as written. Sorted by slug. Not paginated: the file is the whole list. Unknown org → **404**. `service_account_default_role` is resolved: `default_role` when the file leaves it unset. Roles off → `roles: []` and the three role fields `null`.

The org is in the path so that a later per-org role set has an address. Today every org gets the same list.

## Identity catalog labels

The catalog ([`services/identity/routes.yml`](../services/identity/routes.yml)) labels the writes that manage the org. Reads stay unlabeled, except the invite list, which returns live tokens.

| Edge route | Label |
|------------|-------|
| `PATCH /org` | `org:write` |
| `DELETE /org` | `org:delete` |
| `POST /org/members`; `PATCH`, `DELETE /org/members/{member_id}`; `GET`, `POST /org/invites`; `DELETE /org/invites/{invite_id}` | `org:members:write` |
| `POST /org/service-accounts`; `PATCH`, `DELETE /org/service-accounts/{service_account_id}`; `POST /org/service-accounts/{service_account_id}/api-keys`; `DELETE /org/service-accounts/{service_account_id}/api-keys/{key_id}` | `org:service-accounts:write` |

Labels are opaque. These names live in the catalog, not in identity code. An operator who edits the catalog edits the labels.

A member whose role lacks these labels gets **403** on these routes, with any of its keys or sessions. With roles off, these labels stop no one: whoever is a member may manage the org, unless something else decides.

## Not here

- Key or session labels, or any other way to narrow a credential below its principal
- A member with no role while roles are on, or roles for some orgs and not others
- A grant cap: comparing the caller to the role it assigns, or to the member it acts on
- Telling services the caller's role or labels
- Role names or meanings in Plat5 code
- Roles in the route registry, etcd, or the gateway
- More than one role per member
- Per-org custom roles (deferred)
- Reloading the file without a restart, or a roles admin API (deferred)
- Resource authorization (the service, or a policy engine it calls)
