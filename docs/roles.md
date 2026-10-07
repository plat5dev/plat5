# Roles

A deployment's roles. Plat5 ships no role names. The deployment's roles file names each role and says which labels it grants.

Boundary: [`identity-boundary.md`](identity-boundary.md). Identity API: [`identity.md`](identity.md). Route labels: [`routes.md`](routes.md).

## The model

Three things decide what a member credential may call.

| Piece | Says | Owner |
|-------|------|-------|
| Route `required_scopes` | Which labels the route needs (any one of them) | The service's `routes.yml` |
| Role | Which labels the member holds | The deployment's roles file, resolved by identity |
| Credential `scopes` | Which of those labels this key or session carries | Whoever minted it |

Effective scopes are the role's labels intersected with the credential's scopes. The gateway checks effective scopes against the route. It does not see roles.

User credentials on `user` routes have no role. Roles are org membership.

Resource authorization (this project, this document) is not a role. It stays in the service, or in a policy engine the service calls.

## File

`ROLES_FILE` on identity is the path to a YAML file. Identity reads it at boot. Unset means no roles. A file that fails validation refuses boot. To change roles, edit the file and restart identity.

```yaml
roles:
  owner: ["*"]
  admin: [org:write, org:members:write, org:service-accounts:write]
  member: []
creator_role: owner
default_role: member
```

| Field | Rule |
|-------|------|
| `roles` | Map of slug → label list. At least one role. |
| slug | `[a-z0-9:._-]+`, max 64. Opaque to Plat5. |
| label list | Same hygiene as key scopes: `[a-z0-9:._-]+`, max 64 chars, unique. Max 64 labels. `[]` grants no labels. `["*"]` grants every label; `*` is valid only alone. |
| `creator_role` | Required. A slug in `roles`. Assigned to whoever creates an org. |
| `default_role` | Required. A slug in `roles`. Assigned when a write omits `role`. |

Unknown keys refuse boot.

`plat5 init` writes the file above as a starter, and [`compose/roles.yml`](../compose/roles.yml) is the same file. Templates ship their own, with their app's labels. None of them is Plat5's. Edit them.

## Resolution

Identity resolves the role at member key validate and member session validate. The validate `scopes` is the effective set. The gateway never gets the role, only that set.

| Member role | Credential `scopes` | Validate `scopes` |
|-------------|---------------------|-------------------|
| `["*"]` or `NULL` | `null` | `null` |
| `["*"]` or `NULL` | a list | that list |
| a label list | `null` | the role's labels |
| a label list | a list | the intersection |
| a slug not in the file | any | `[]` |

`*` does not leave identity. On the wire, unrestricted is still `null` and an absent `X-Plat5-Scopes`.

A `NULL` role is unrestricted. That is every member when there is no roles file, and every row written before there was one. With a roles file, every new member row gets a role. To tighten existing members, assign them roles.

A slug removed from the file grants nothing, so members who still hold it fail closed.

The gateway caches validate for `APIKEY_CACHE_TTL_SECS`. A role change is visible at the edge when that TTL expires, like a suspend. It applies to existing keys and sessions, because validate intersects again every time.

## Assigning

| Write | Role |
|-------|------|
| `POST /users/{user_id}/organizations` | `creator_role`. No body field. |
| `POST /organizations/{organization_id}/members` | `role` in the body, or `default_role` |
| `POST /organizations/{organization_id}/invites` | `role` in the body, or `default_role`. Stored on the invite. Redeem assigns it. |
| `POST /organizations/{organization_id}/service-accounts` | `role` in the body, or `default_role` |
| `PATCH /organizations/{organization_id}/members/{member_id}` | `role` in the body |

Without a roles file, `role` in a body is **422**. With one, a slug not in the file is **422**.

## Grant cap

The caller's effective scopes arrive on `X-Plat5-Scopes`, as they do for key mint. A caller cannot hand out more than it holds, and cannot act on a member who holds more than it does.

- Assigning a role needs every label of that role. A `["*"]` role needs an unrestricted caller.
- Changing, suspending, or removing another member through the org address needs every label of that member's current role. The same holds for a service account, and for minting or revoking its keys.
- Org create is not capped. The creator is the first member.
- Redeem is not capped. The invite's creator was, at create.

A miss is **403** `INSUFFICIENT_SCOPE`. `details.scopes` lists the missing labels (`["*"]` for an unrestricted role).

An unrestricted caller (no header) passes. Without a roles file, a member credential is unrestricted unless its own scopes narrow it, so the cap is the key mint cap and nothing more.

## Last creator role

A write may not take an org's count of non-removed members holding `creator_role` from one to zero. That covers a demotion, a remove at either address, and a service-account delete. **422**. Suspending is allowed, as it is for the last member. Deleting the org is not blocked.

## Keys minted by a role-restricted caller

Key mint copies the caller's effective scopes when `scopes` is omitted ([`identity.md`](identity.md)). For a caller whose role has a label list, that copy is a snapshot. The key does not gain labels the role gains later. It still loses labels the role loses, because validate intersects again. A caller whose effective scopes are unrestricted mints `NULL`, which follows the role.

## Listing roles

`GET /organizations/{organization_id}/roles` returns the deployment's roles. The catalog publishes it as `GET /org/roles`, unlabeled, so a console can show a role picker.

```json
{
  "roles": [
    { "slug": "admin", "scopes": ["org:write", "org:members:write", "org:service-accounts:write"] },
    { "slug": "member", "scopes": [] },
    { "slug": "owner", "scopes": null }
  ],
  "creator_role": "owner",
  "default_role": "member"
}
```

`scopes: null` is `["*"]`. Sorted by slug. Not paginated: the file is the whole list. Unknown org → **404**. No roles file → `roles: []` and both fields `null`.

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

A restricted key without these labels gets **403** on these routes. That includes keys minted before the catalog carried the labels.

## Not here

- Role names or meanings in Plat5 code
- Roles in the route registry, etcd, or the gateway
- More than one role per member
- Per-org custom roles (deferred)
- Reloading the file without a restart, or a roles admin API (deferred)
- Resource authorization (the service, or a policy engine it calls)
