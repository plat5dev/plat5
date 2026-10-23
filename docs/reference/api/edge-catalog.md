# Edge catalog

The routes identity and audit expose at the gateway, generated per kind. Concept: [access](../../concepts/access.md).

Identity's edge routes, and the audit service's read routes, are generated from `kinds` ([generated routes](#generated-routes)). `POST /catalog` takes the kinds in the body, so the catalog for a new kind can be generated before that kind is applied, and both go in one apply. `plat5 routes generate` writes it to `catalog.yml`. The team edits it like any routes file (renames labels, drops routes) and keeps it in the config. Omitting a route hides it from the gateway; the identity process still serves that path on the network.

## Generated routes

Identity's edge routes are generated from the `kinds` document (`plat5 routes generate`, or `POST /catalog` on the control plane) and kept in the config like any routes file ([`reference/api/edge-catalog.md`](edge-catalog.md)). A new kind gets its API without code. Each kind gets a path segment (its `segment`, or its name plus `s`) and the labels below. Leaving a route out of the applied catalog, such as `POST /orgs`, is how a deployment restricts it.

## Generated labels

| Label | Checked at | Allows |
|-------|------------|--------|
| `<kind>:read` | The node | Getting the node; the node appearing in lists |
| `<kind>:create` | The parent | Creating one under the parent |
| `<kind>:write` | The node | Changing the label |
| `<kind>:delete` | The node | Deleting, listing deleted, restoring |
| `<kind>:bindings:read` / `:write` | The node | Listing or changing who has access here. On a root, also adding members and invites to join |
| `<kind>:service-accounts:write` | The node | Creating service accounts here and their keys |
| `<kind>:audit:read` | The node | Reading audit for this node and below |
| `<root kind>:guests:write` | The root | Invites on nodes below the root, which can admit people outside the tree. Granted to no role (counting `*`), guests are off |

**These are defaults, not rules.** They're the labels written into the generated catalog; the team can rename them, change them or drop routes. Identity's code depends on no label name ([filtered lists](../lists.md#filtered-lists)). Roles grant them like any label; `*` grants all.

## Shown for `project` under `organization` (segment `orgs`)

| Edge method and path | Labels | Upstream on identity | Notes |
|---|---|---|---|
| `GET /orgs/{organization}/projects` | `[]` | `GET /nodes/{subject.organization_id}/children/project` | [Filtered](../lists.md#filtered-lists). `?state=deleted` lists deleted ones the caller could delete |
| `POST /orgs/{organization}/projects` | `project:create` | `POST /nodes/{subject.organization_id}/children/project` | Body `{ label, slug? }`. The caller gets `creator_role`, unless an inherited binding already holds it |
| `GET /orgs/{organization}/projects/{project}` | `project:read` | `GET /nodes/{subject.project_id}` | |
| `PATCH …/{project}` | `project:write` | `PATCH /nodes/{subject.project_id}` | `{ label }`. The slug never changes |
| `DELETE …/{project}` | `project:delete` | `DELETE /nodes/{subject.project_id}` | Soft; cascades; revokes keys of service accounts born here and below |
| `POST …/{project}/restore` | `project:delete` | `POST /nodes/{subject.project_id}/restore` | `resolve_deleted: true` ([`reference/config/routes.md`](../config/routes.md#route-fields)). **409** if the parent is deleted |
| `GET, POST …/{project}/bindings` | `project:bindings:read`, `:write` | `/nodes/{subject.project_id}/bindings` | Body `{ member, role, expires_at? }`; `member` is a member id in the org |
| `PATCH, DELETE …/{project}/bindings/{binding_id}` | `project:bindings:write` | `/nodes/{subject.project_id}/bindings/{path.binding_id}` | Change role or expiry; remove |
| `GET …/{project}/access` | `project:bindings:read` | `GET /nodes/{subject.project_id}/access` | Direct and inherited bindings, each with the node it comes from |
| `GET, POST …/{project}/service-accounts` | `project:bindings:read`, `project:service-accounts:write` | `/nodes/{subject.project_id}/service-accounts` | `{ name, role }`: born here, bound here |
| `GET, PATCH, DELETE …/service-accounts/{service_account}` | `project:bindings:read`, `project:service-accounts:write` | `/nodes/{subject.project_id}/service-accounts/{path.service_account}` | `{ name }`. Delete removes its member and revokes its keys |
| `GET, POST …/service-accounts/{service_account}/keys` | `project:service-accounts:write` | `…/service-accounts/{path.service_account}/keys` | Plaintext once |
| `DELETE …/keys/{key_id}` | `project:service-accounts:write` | `…/keys/{path.key_id}` | Revoke |
| `GET, POST …/{project}/invites` | `organization:guests:write` | `/nodes/{subject.project_id}/invites` | `{ role, role_duration?, max_uses?, expires_at }`. Can admit a guest. To give someone already in the org a project role, bind them directly |
| `DELETE …/invites/{invite_id}` | `organization:guests:write` | `/nodes/{subject.project_id}/invites/{path.invite_id}` | Revoke |
| `GET …/{project}/roles` | `[]` | `GET /nodes/{subject.project_id}/roles` | Roles bindable here |

Roots get the same at `/orgs/{organization}`, with `organization:*` labels, except invites, which are the root's own (below). Plus:

| Edge method and path | Scope, labels | Upstream on identity | Notes |
|---|---|---|---|
| `POST /orgs` | `directory`, `[]` | `POST /roots/organization` | `{ label, slug? }`. Creates a root; the caller gets a member and `creator_role` |
| `GET, POST /orgs/{organization}/members` | `organization:bindings:read`, `:write` | `/nodes/{subject.organization_id}/members` | Post: `{ idp: { issuer, subject }, role?, expires_at? }`. `role` omitted → `default_role` |
| `GET /orgs/{organization}/members/{member}` | `[]` | `GET /nodes/{subject.organization_id}/members/{path.member}` | Name and status, a removed member too. So services and consoles can show "Dana (former member)" |
| `DELETE /orgs/{organization}/members/{member}` | `organization:bindings:write` | `DELETE /nodes/{subject.organization_id}/members/{path.member}` | Remove |
| `DELETE /orgs/{organization}/me` | `[]` | `DELETE /nodes/{subject.organization_id}/members/{subject.member_id}` | Leave the org |
| `GET, POST /orgs/{organization}/invites` | `organization:bindings:write` | `/nodes/{subject.organization_id}/invites` | `{ role?, role_duration?, grants?, max_uses?, expires_at }`. `grants` are bindings below the root made on redeem |
| `DELETE /orgs/{organization}/invites/{invite_id}` | `organization:bindings:write` | `/nodes/{subject.organization_id}/invites/{path.invite_id}` | Revoke |

The caller's own routes, all `scope: directory` with `[]`:

| Edge method and path | Upstream on identity | Notes |
|---|---|---|
| `GET /me` | `GET /members/{subject.member_id}` | The person: name, status |
| `DELETE /me` | `DELETE /members/{subject.member_id}` | Delete the account ([lifecycle](../../concepts/lifecycle.md#deleting-an-account)) |
| `GET /me/nodes` | `GET /members/{subject.member_id}/nodes` | `?kind=project`: every node of that kind the person can read, across trees, with slug, label and the root's slug |
| `GET, POST /me/keys` | `/members/{subject.member_id}/keys` | Personal keys. They act as the person everywhere |
| `DELETE /me/keys/{key_id}` | `/members/{subject.member_id}/keys/{path.key_id}` | Revoke |
| `POST /me/invites/redeem` | `POST /members/{subject.member_id}/invites/redeem` | `{ token }` |

Service accounts have no directory member and can't call these.

The catalog also holds the audit service's read routes: `GET …/audit-events` at every kind with `<kind>:audit:read`, upstream `/nodes/{subject.<kind>_id}/audit-events`, and `GET /me/audit-events`, upstream `/members/{subject.member_id}/audit-events` ([`reference/api/audit.md`](audit.md#reading)).
