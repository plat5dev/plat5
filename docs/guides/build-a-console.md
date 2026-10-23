# Build a console

A console is a client of the gateway's edge routes, like a CLI or a script. It holds the person's IdP token and calls routes by slug.

## Signing in and the person

The console gets an IdP token and sends it as `Authorization: Bearer`. There are no Plat5 sessions: the same token reaches every tree the person is in ([access](../concepts/access.md#credentials)). `GET /me` is the person; `GET /me/nodes?kind=organization` is every org they can read, for an org switcher.

## Navigating the tree

Paths carry slugs: `/orgs/acme/projects/web/…`. Lists show only what the caller can read, so a project-only member sees their projects, not a 403 ([lists](../concepts/access.md#lists)). A filtered page can be short with `has_more: true`; keep walking ([`reference/lists.md`](../reference/lists.md)).

## Errors

Show `error.message` ([`reference/errors.md`](../reference/errors.md)). A 403 never says whether a node exists, so the console shouldn't either: "Permission denied, or it may not exist." `details.required_labels` says which labels would have worked, for a "ask an admin for…" hint.

## Who has access

- `GET …/access` lists direct and inherited bindings, each with where it comes from; guests are marked.
- `GET …/roles` lists the roles bindable at that node, for a role picker ([`reference/api/identity.md`](../reference/api/identity.md#roles)).
- Binding writes take a member id in the org; a role with a `max_duration` needs an `expires_at`.

## Inviting

- To the org: `POST /orgs/{organization}/invites`, with `grants` for projects bound on redeem.
- To one project, which can bring in a guest: `POST …/projects/{project}/invites`, if the deployment turned guests on.
- The invitee redeems with `POST /me/invites/redeem`. Plat5 sends no email; the console does.

## Deleting and restoring

Deletes are soft for the deployment's grace period. `?state=deleted` on a list shows what the caller could restore, and `POST …/restore` brings it back ([lifecycle](../concepts/lifecycle.md#deleting-restoring-and-purging)). A deleted org is found with `GET /me/nodes?kind=organization&state=deleted`.

Every route the console can call: [`reference/api/edge-catalog.md`](../reference/api/edge-catalog.md).
