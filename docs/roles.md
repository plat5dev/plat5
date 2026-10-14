# Roles

A deployment's roles. Plat5 ships no role names. The deployment's roles document names each role and says which labels it grants. The operator writes it to the control plane; identity reads it from there and picks up each change without a restart.

Boundary: [`identity-boundary.md`](identity-boundary.md). Identity API: [`identity.md`](identity.md). Route labels: [`routes.md`](routes.md).

## The model

Two things decide what a member credential may call.

| Piece | Says | Owner |
|-------|------|-------|
| Route `required_labels` | Which labels the route needs (any one of them) | The service's `routes.yml` |
| Role | Which labels the member holds | The deployment's roles document, resolved by identity |

Roles are on or off for the whole deployment. There is no mix.

| | Roles on (default) | Roles off (`ROLES_ENABLED=false`) |
|---|---|---|
| Member `role` | A slug from the document. Writes always set one | Always `null` |
| Validate and session mint `labels` | The role's list as the document writes it | `["*"]` |
| Route `required_labels` | Checked | Never blocks a member |
| `role` in a request body | A slug in the document, or **422** | **422** |
| [Last creator role](#last-creator-role) | Enforced | Nothing to keep |
| `GET /organizations/{organization_id}/roles` | The document | `roles: []`, the three role fields `null` |

Roles off is for a deployment where something else decides which routes a member may call, such as a policy engine. Every member then holds every label, so route labels, including the identity catalog's, stop no one.

A credential belongs to a membership and carries that membership's permissions. A member key or session carries the member's role labels. Nothing narrows it: keys and sessions have no labels of their own. To give a machine less, give it a service account with a smaller role.

The gateway checks the member's labels against the route. It does not see roles. Services see neither: the gateway is the only place this level of access is decided.

User credentials on `user` routes have no role. A person's membership in the directory has none; roles are org membership.

Resource authorization (this project, this document) is not a role. It is the optional policy engine's: the gateway asks it about one resource, and a service asks it which resources its subject can reach ([`model.md`](model.md#what-i-can-reach)).

## On or off

`ROLES_ENABLED` on identity: on unless `false`. It is a boot switch, and the only one. Turning roles on leaves every existing member without a role, which grants nothing, so it is a migration, not a live change. With roles off, identity logs a warning at boot.

With roles on, identity needs `CONTROL_PLANE_URL`. It does not listen until it has loaded the roles document, retrying while the control plane is down or has none. A roles document that is not loaded yet never reads as roles off: off grants every label. Once loaded, identity keeps the last document it has while the control plane is down.

## The roles document

The operator writes it to the control plane with `PUT /roles` (`plat5 roles apply roles.yml`). The control plane validates it, keeps every revision, and can restore one ([`control-plane.md`](control-plane.md#documents)). Identity long-polls for changes and swaps the whole set when one lands, usually within a second or two. One request reads one document.

The body is the roles file, in YAML or JSON:

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

A document that breaks a rule, or has an unknown key, is **422** at the control plane and is never stored. Identity checks it again on receipt; one it cannot use is skipped, and it keeps the one it has.

Compose seeds [`compose/roles.yml`](../compose/roles.yml) as the first document when there has never been one (`SEED_ROLES_FILE`), so a new deployment can start. After that, the file is not read again: change roles with `PUT /roles`. `plat5 init` writes the same file as a starter, and `plat5 start` applies it. Templates ship their own, with their app's labels. None of them is Plat5's. Edit them.

## Resolution

Identity resolves the role at credential validate, for a member key or session. The validate `labels` is the role's labels. The gateway never gets the role, only its labels.

| Member role | Validate `labels` |
|-------------|-------------------|
| `["*"]` | `["*"]` |
| a label list | that list |
| a slug not in the document | `[]` |
| any, roles off | `["*"]` |

`labels` is always a list, and it says what the document says. `*` is every label: the gateway matches it to any route label. A route cannot require `*` itself.

A slug removed from the document grants nothing, so members who still hold it fail closed. The control plane cannot see members, so it does not stop that removal.

The gateway caches validate for `APIKEY_CACHE_TTL_SECS`. A member's role change, or a change to the roles document, is visible at the edge when that TTL expires, like a suspend. A roles document change first reaches identity, within a second or two. It applies to existing keys and sessions, because validate resolves the role every time. A key follows its member's role: it gains labels the role gains, and loses labels the role loses.

## Assigning

| Write | Role |
|-------|------|
| `POST /users/{user_id}/organizations` | `creator_role`. No body field. |
| `POST /organizations/{organization_id}/members` | `role` in the body, or `default_role` |
| `POST /organizations/{organization_id}/invites` | `role` in the body, or `default_role`. Stored on the invite. Redeem assigns it. |
| `POST /organizations/{organization_id}/service-accounts` | `role` in the body, or `service_account_default_role` (unset → `default_role`) |
| `PATCH /organizations/{organization_id}/members/{member_id}` | `role` in the body |

A slug not in the document is **422**. With roles off, every write above leaves `role` empty, and `role` in a body is **422**.

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

`labels` is the document's list as written. Sorted by slug. Not paginated: the document is the whole list. Unknown org → **404**. `service_account_default_role` is resolved: `default_role` when the document leaves it unset. Roles off → `roles: []` and the three role fields `null`.

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
- Roles in the gateway. The control plane stores the document; only identity reads it
- More than one role per member
- Per-org custom roles (deferred)
- Turning roles on or off without a restart
- Resource authorization (the optional policy engine, [`model.md`](model.md))
