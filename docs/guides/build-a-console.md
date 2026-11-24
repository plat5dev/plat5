# Build a console

A console is a client of the gateway's routes, like a CLI or a script. It holds the person's IdP token and calls routes by slug. The paths below follow identity's [reference routes](../reference/config/generated-routes.md).

## Signing in and the person

Send the IdP's access token as `Authorization: Bearer`. There are no Plat5 sessions: the same token reaches every tree the person is in ([access](../concepts/access.md#credentials)).

- `GET /user` returns the person (`name` is `null` until known).
- `PATCH /user { name }` sets or clears their name ([a person's name](../reference/idp.md#a-persons-name)).
- `GET /user/nodes?kind=organization` lists every org they can see, for an org switcher. Each node carries its `ancestors`, so the console builds its own URLs from the chain.

## Navigating the tree

Paths carry slugs: `/orgs/acme/projects/web/…`. Lists return only what the caller can see, so a project-only member sees their projects rather than a 403 ([lists](../concepts/access.md#lists)). A filtered page can return fewer than `limit` items with `has_more: true`; keep walking until `has_more` is `false` ([`reference/lists.md`](../reference/lists.md)).

## What to show

Each page or action maps to the request it needs. Ask the gateway once per page or navigation in a single batch at `POST /access/routes`:

```json
POST /access/routes
{ "checks": [
  { "method": "GET", "path": "/orgs/acme/bindings" },
  { "method": "POST", "path": "/orgs/acme/projects" },
  { "method": "DELETE", "path": "/orgs/acme/projects/web" }
] }
→ { "checks": [ { "allowed": false }, { "allowed": true }, { "allowed": false } ] }
```

Ask by route rather than permission name so permission renames in config don't break the UI (up to 100 checks per call, [`reference/gateway.md`](../reference/gateway.md#route-checks)).

`GET …/membership` on a node returns the caller's member id, active bindings, and permissions — use it to show "Admin" or "Guest" and mark "you" in member lists.

Both answers are advisory: the gateway still enforces every request when it is made.

## Errors

Display `error.message` ([`reference/errors.md`](../reference/errors.md)). Through the gateway, a missing node and a denied one return the same **403**; on permission-gated routes, `details.required_permissions` lists which permissions would have admitted the call.

## Nodes with service fields

When a creation form includes product-specific fields owned by one of your services, submit in two steps: create the node through identity (`POST /orgs/acme/projects { display_name }`), then `PUT` the service's fields to your service's route for the new node. If the second call fails, keep the form open and retry only the idempotent `PUT`.

## Who has access

- `GET …/bindings` lists every binding that counts at the node (member, group, and public), with `inherited`, `guest`, and `expires_at` fields. A member or group can hold multiple distinct roles on the same node.
- `GET /orgs/{organization}/members` lists the org's non-removed members (active and suspended), guests marked. Use `PATCH /orgs/{organization}/members/{member_id}/status` to reversibly suspend or reactivate a member without rotating their `member_id`.
- `GET /orgs/{organization}/groups` and `…/groups/{group_id}/members` manage in-tree groups; bind a group on any node just like a member.
- `GET …/roles` lists system and custom roles bindable at that node, for role pickers (`GET /orgs/{organization}/roles?all=true` returns every role in the org). When `custom_roles: true`, use `GET /orgs/{organization}/permissions` to populate a custom-role editor and `POST`/`PATCH`/`DELETE /orgs/{organization}/roles` to manage custom roles.
- `GET`/`PATCH /orgs/{organization}/security-policy` configures org-wide `allow_personal_keys`, `allowed_cidrs`, and `required_idp_claims`.
- `GET`/`POST`/`PATCH`/`DELETE /user/keys` and `…/service-accounts/{id}/keys` manage API keys, including optional `allowed_cidrs`, `scopes`, `allowed_roots` (personal keys), and `expires_at` / `expires_in`.
- Removing a person's last binding in a tree permanently removes their member ([members](../concepts/lifecycle.md#members)). To move someone's only access, add the new binding before removing the old one.
- Adding a member by IdP subject (`POST /orgs/{organization}/members`) is not exposed in the default routes; offer it only when `POST /access/routes` answers `true` for it.

## Inviting

- To the org: `POST /orgs/{organization}/invites`, with `role` and optional `grants` for child nodes.
- To one child node (can admit a guest): `POST …/projects/{project}/invites`, when `guests:write` is granted.
- The invitee redeems with `POST /user/invites/redeem`; the response returns the target node with its ancestors. Plat5 sends no email.

## Deleting, restoring, and leaving

- Node deletes are soft until `purge_after`. List deleted nodes with `?status=deleted`, and restore with `POST …/restore` ([lifecycle](../concepts/lifecycle.md#deleting-restoring-and-purging)). Find deleted orgs with `GET /user/nodes?kind=organization&status=deleted`.
- `DELETE /orgs/{organization}/membership` leaves an org. `DELETE /user` deletes the account.
