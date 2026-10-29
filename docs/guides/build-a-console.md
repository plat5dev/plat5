# Build a console

A console is a client of the gateway's routes, like a CLI or a script. It holds the person's IdP token and calls routes by slug. The routes below are the defaults ([resources](../reference/config/resources.md)); `plat5 routes list` shows a deployment's own.

## Signing in and the person

The console gets an IdP token and sends it as `Authorization: Bearer`. There are no Plat5 sessions: the same token reaches every tree the person is in ([access](../concepts/access.md#credentials)). `GET /user` is the person, with a `name` that's `null` until it's known; `PATCH /user { name }` lets them set it, unless the deployment's IdP owns names, which is a **409** ([a person's name](../reference/idp.md#a-persons-name)). `GET /user/nodes?kind=organization` is every org they can see, for an org switcher. Each node carries its `ancestors`, so the console builds its URLs from the chain.

## Navigating the tree

Paths carry slugs: `/orgs/acme/projects/web/…`. Lists show only what the caller can see, so a project-only member sees their projects, not a 403 ([lists](../concepts/access.md#lists)). A filtered page can be short with `has_more: true`; keep walking ([`reference/lists.md`](../reference/lists.md)).

## What to show

Each page names its main call: the one request it can't work without, such as `GET /orgs/{organization}/bindings` for an org's access page. A page is hidden, with its links, when its main call isn't allowed; a button is hidden when its request isn't.

Ask the gateway once per page or navigation, in one batch: every main call the page links to and every button's request, by method and path, sent to the deployment's `route_checks.path` (`POST /access/routes` by default). The answers come back in order ([route checks](../reference/gateway.md#route-checks)):

```
POST /access/routes
{ "checks": [
  { "method": "GET", "path": "/orgs/acme/bindings" },
  { "method": "POST", "path": "/orgs/acme/projects" },
  { "method": "DELETE", "path": "/orgs/acme/projects/web" }
] }
→ { "checks": [ { "allowed": false }, { "allowed": true }, { "allowed": false } ] }
```

Ask by route, not by permission name: the deployment names permissions and can rename them. Up to 100 checks a call.

`GET …/membership` on a node says who the caller is there: their member id, the bindings that count, and the permissions they grant. A console shows "Admin" or "Guest" from it, and marks "you" in member lists.

Both answers are advisory ([what a UI shows](../concepts/access.md#what-a-ui-shows)). The request is still decided when it's made, so a page shown on a stale answer gets a **403**, shown like any other.

## Errors

Show `error.message` ([`reference/errors.md`](../reference/errors.md)). A 403 never says whether a node exists, so the console shouldn't either: "Permission denied, or it may not exist." On a route that needs permissions, `details.required_permissions` says which would have worked, for an "ask an admin for…" hint ([`FORBIDDEN`](../reference/errors.md#forbidden)).

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
