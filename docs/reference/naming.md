# Naming Conventions

Formats and terminology for permissions, routes, metrics, and error codes across Plat5.

## Project Terminology

- **Plat5** — Brand and main product: gateway, identity, control plane, contracts (platform runtime).
- **Plat5 Auth** — Optional reference OIDC IdP (separate product).
- **Service** — Any unit of code that runs with Plat5 (e.g. `gateway`, `identity`, `widgets`).
- **Platform service** — Plat5 runtime units: `gateway`, `control-plane`, `identity`, `audit`.
- **Business service** — Use-case service behind Plat5: domain APIs.
- **IdP** — External identity provider (JWT issuer). Not part of Plat5 (optional Plat5 Auth is a separate product).

### Scope of Plat5

Plat5 owns the tree of nodes (kinds from config), the principals who can reach them (people linked to IdP subjects, and service accounts), their members and bindings, API keys, audit, the control plane (kinds, roles, routes, CORS origins), and the user-facing gateway. Login is the IdP. Everything inside a node is the services'. See [`concepts/model.md`](../concepts/model.md).

### Identity domain nouns

| Concept | Formal name | Notes |
|---------|-------------|--------|
| A level of the tree | **Kind** | Declared in the `kinds` document: `organization`, `project`, … One parent kind, or none |
| One instance of a kind | **Node** | Has an id, a slug, a display name and a status |
| A node with no parent | **Root** | Its tree is the isolation boundary. An organization is a root. Do not call it tenant |
| A person or a service account | **Principal** | Its id never leaves identity |
| A person on their own routes | **User** | The user id is sent only on user routes. Private: customers can't list or search people |
| A principal in one tree | **Member** | The id services see as the caller on tree routes |
| A role on a member at a node | **Binding** | Access is the union of bindings on the node and its ancestors |
| A person's member with no binding at its root | **Guest** | Derived, not stored. Never a service account |
| What a binding holds | **Role** | Deployment-defined slug; its permissions come from the roles document ([`concepts/access.md`](../concepts/access.md)). Plat5 names no roles |
| Route requirement | **Permission** | Opaque `[a-z0-9:._-]+`. Routes require them (`required_permissions`); roles grant them |
| What a route addresses | **Scope** | A kind name, `user`, or `public` |
| Non-human principal | **Service account** | Born at a node of an org's tree |
| Credential | **API key** | A person's personal key, or a service account's key. No sessions |

**Rejected service names:** `org-service`, `orgs`, `tenants`, `tenancy`, `rbac`, `authz`, `accounts`, `api-keys` (as a standalone platform service).

The identity service is **`identity`** (`service.name: identity`).

## Service Names

- Format: `kebab-case`
- Examples: `identity`, `gateway`, `control-plane`
- Match the service directory name
- Used in `service.name` container label and OTel resource attribute

## Service Namespaces

Full table and labels: [`reference/container-labels.md`](container-labels.md). Identity backend uses **`identity`**; gateway uses **`edge`**.

| Service | `service.namespace` |
|---------|---------------------|
| `identity` | `identity` |
| `gateway`, `control-plane` | `edge` |
| `audit` | `audit` |
| Business APIs | `api` (etc.) |

When changing labels: update compose labels and `OTEL_SERVICE_NAMESPACE` together.

## HTTP Routes

- `kebab-case` path segments: `/user-profiles`
- No path version segment. No `/api` prefix.
  - Public: `/public/...`
  - Internal (private network, not on the gateway): `/internal/...`
- Resource names are plural nouns: `/widgets`, `/nodes`, `/members`
- Identity list query/body: [`reference/lists.md`](lists.md). Business services may copy it; they are not required to.
- Actions use HTTP methods, not verbs in paths:
  - `POST /widgets` — create
  - `GET /widgets/{id}` — read
  - `PUT /widgets/{id}` — update
  - `DELETE /widgets/{id}` — delete

### Path patterns

Services address everything by id. Identity:

| Surface | Pattern |
|---------|---------|
| A node | `/nodes/{node_id}/...` |
| A new root | `/roots/{kind}` |
| A person | `/users/{user_id}/...` |
| Internal (not on the gateway) | `/internal/admit`, `/internal/plan`, `/internal/lifecycle-events` |

Audit follows the same pattern: `/nodes/{node_id}/audit-events` and `/users/{user_id}/audit-events` public, `/internal/events/{request_id}` internal ([`concepts/audit.md`](../concepts/audit.md)).

Public paths carry slugs: each kind's `path` piece, root first, then the route's own path relative to the node:

| Public path | Scope | Upstream placeholders |
|------|-------|----------------|
| `/orgs/{organization}/...` | `organization` | `{organization.id}`, `{member.id}` |
| `/orgs/{organization}/projects/{project}/...` | `project` | `{organization.id}`, `{project.id}`, `{member.id}` |
| `/user/...` | `user` | `{user.id}` |

The route's own params are bare in both: `{plan_id}`. There is no `/api` prefix. Slugs never reach a service. Full contract: [`reference/config/services.md`](config/services.md), [`reference/gateway.md`](gateway.md).

## Subject headers

Tree routes: `X-Plat5-<Kind>-Id` per node in the chain (title case, `_` → `-`), `X-Plat5-Member-Id`, `X-Plat5-Principal`. User routes: `X-Plat5-User-Id`, `X-Plat5-Principal`. Always: `X-Request-ID`, `traceparent`. Services are never told a principal id, or the caller's role or permissions.

## Log Fields

- Format: `snake_case`
- Common: `request_id`, `root_id`, `node_id`, `member_id`, `user_id`, `duration_ms`, `error_kind`
- Credential (gateway request line): `auth_type`, `key_id` (never the key). See [`reference/telemetry.md`](telemetry.md#gateway-request-line)
- Service-specific fields should be namespaced: `auth_provider`, `db_operation`

## Metric Names

- Format: `snake_case`
- Structure: `<domain>_<entity>_<unit>`
- Examples: `http_requests_total`, `http_request_duration_seconds`, `db_operations_total`, `process_resident_memory_bytes`

## Error Codes

- Format: `UPPER_SNAKE_CASE`
- Full list: [`reference/errors.md`](errors.md)

## Environment Variables

- Format: `UPPER_SNAKE_CASE`
- Group by prefix when related: `OTEL_SERVICE_NAME`, `OTEL_EXPORTER_OTLP_ENDPOINT`
- Examples: `PORT`, `DATABASE_URL`, `DEPLOYMENT_ENV`

## Database

- Table names: `snake_case`, plural: `nodes`, `members`, `bindings`, `api_keys`
- Column names: `snake_case`: `created_at`, `updated_at`, `node_id`, `root_id`, `member_id`
- Foreign key columns: `<entity>_id`
- New row ids: lowercase **ULID** strings, no prefix
- A service keys its data by node id (`project_id`), and authored records by member id

## Related

| Doc | Role |
|-----|------|
| [`concepts/model.md`](../concepts/model.md) | Concepts, rules, what Plat5 won't do |
| [`reference/api/identity.md`](api/identity.md) | Identity service API |
| [`reference/container-labels.md`](container-labels.md) | Namespace values |
| [`reference/gateway.md`](gateway.md) | Headers and service rules |
| [`reference/config/services.md`](config/services.md) | Service files, route scopes and placeholders |
| [`reference/lists.md`](lists.md) | Identity list pagination (optional for business APIs) |
