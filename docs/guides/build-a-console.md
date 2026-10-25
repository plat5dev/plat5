# Build a console

A console is a client of the gateway's routes, like a CLI or a script. It holds the person's IdP token and calls routes by slug. The routes below are the defaults ([resources](../reference/config/resources.md)); `plat5 routes list` shows a deployment's own.

## Signing in and the person

The console gets an IdP token and sends it as `Authorization: Bearer`. There are no Plat5 sessions: the same token reaches every tree the person is in ([access](../concepts/access.md#credentials)). `GET /user` is the person; `GET /user/nodes?kind=organization` is every org they can see, for an org switcher. Each node carries its `ancestors`, so the console builds its URLs from the chain.

## Navigating the tree

Paths carry slugs: `/orgs/acme/projects/web/…`. Lists show only what the caller can see, so a project-only member sees their projects, not a 403 ([lists](../concepts/access.md#lists)). A filtered page can be short with `has_more: true`; keep walking ([`reference/lists.md`](../reference/lists.md)).

## What the caller can do here

`GET …/membership` on any node returns the caller's member id, the bindings that count there, and the permissions they grant. A console shows "Admin" or "Guest", marks "you" in member lists, and hides buttons the caller can't use.

## Errors

Show `error.message` ([`reference/errors.md`](../reference/errors.md)). A 403 never says whether a node exists, so the console shouldn't either: "Permission denied, or it may not exist." `details.required_permissions` says which permissions would have worked, for an "ask an admin for…" hint.

## Who has access

- `GET …/bindings` lists every binding that counts at the node, each with where it sits and whether it's inherited; guests are marked.
- `GET /orgs/{organization}/members` lists the org's members, guests marked.
- `GET …/roles` lists the roles bindable at that node, for a role picker ([`reference/api/identity.md`](../reference/api/identity.md#roles)).
- Binding writes take a member id in the org and a role.

## Inviting

- To the org: `POST /orgs/{organization}/invites`, with `role` and `grants` for projects bound on redeem.
- To one project, which can bring in a guest: `POST …/projects/{project}/invites`, if the deployment turned guests on.
- The invitee redeems with `POST /user/invites/redeem`; the answer has the node to go to. Plat5 sends no email; the console does.

## Deleting and restoring

Deletes are soft for the deployment's grace period. A deleted node's `purge_after` says until when it can be restored. `?status=deleted` on a list shows deleted nodes, and `POST …/restore` brings one back ([lifecycle](../concepts/lifecycle.md#deleting-restoring-and-purging)). A deleted org is found with `GET /user/nodes?kind=organization&status=deleted`.

## Leaving

`DELETE /orgs/{organization}/membership` leaves the org. `DELETE /user` deletes the account.
