# Naming Conventions

Formats and terminology for labels, routes, metrics, and error codes across Plat5.

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
| A level of the tree | **Kind** | Declared in the `kinds` document: `organization`, `project`, … |
| One instance of a kind | **Node** | Wire id `n_…`. Has a slug, a label and a status |
| A node with no parent | **Root** | Its tree is the isolation boundary. An organization is a root. Do not call it tenant |
| Where every person signs in | **Directory** | The sign-in kind's single node. Private: customers can't list or search people |
| A person or a service account | **Principal** | Its id never leaves identity |
| A principal in one tree | **Member** | Wire id `m_…`. In the directory, a person's member id is their **user id** |
| A role on a member at a node | **Binding** | Wire id `b_…`. Access is the union of bindings on the node and its ancestors |
| A member with no binding at its root | **Guest** | Derived, not stored |
| What a binding holds | **Role** | Deployment-defined slug; its labels come from the roles document ([`concepts/access.md`](../concepts/access.md)). Plat5 names no roles |
| Route requirement | **Label** | Opaque `[a-z0-9:._-]+`. Routes require them (`required_labels`); roles grant them |
| What a route addresses | **Scope** | A kind name, or `public` |
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

Full table and labels: [`reference/container-labels.md`](container-labels.md). Identity backend uses **`identity`**; gateway stays **`edge`**.

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
| A person (their directory member) | `/members/{member_id}/...` |
| Internal (not on the gateway) | `/internal/admit`, `/internal/plan`, `/internal/lifecycle-events` |

Audit follows the same pattern: `/nodes/{node_id}/audit-events` and `/members/{member_id}/audit-events` public, `/internal/events/{request_id}` internal ([`concepts/audit.md`](../concepts/audit.md)).

Edge paths carry slugs, one param per kind named as the kind, and the kind's `segment`:

| Edge | Scope | Upstream fills |
|------|-------|----------------|
| `/orgs/{organization}/...` | `organization` | `{subject.organization_id}`, `{subject.member_id}` |
| `/orgs/{organization}/projects/{project}/...` | `project` | `{subject.organization_id}`, `{subject.project_id}`, `{subject.member_id}` |
| `/me/...` | the sign-in kind | `{subject.member_id}`: the person's user id |

There is no `/api` prefix. Slugs never reach a service. Full contract: [`reference/config/routes.md`](config/routes.md), [`reference/gateway.md`](gateway.md).

## Subject headers

`X-Plat5-<Kind>-Id` per node in the chain (title case, `_` → `-`), `X-Plat5-Member-Id`, `X-Plat5-Principal`. Always: `X-Request-ID`, `traceparent`. Services are never told a person's principal id, or the caller's role or labels.

## Log Fields

- Format: `snake_case`
- Common: `request_id`, `root_id`, `node_id`, `member_id`, `duration_ms`, `error_kind`
- Credential (gateway request line): `auth_type`, `key_prefix` (display prefix, never the key). See [`reference/telemetry.md`](telemetry.md#gateway-request-line)
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
- New row ids: prefixed **ULID** strings (`n_`, `m_`, `b_`, `k_`, `i_`)
- A service keys its data by node id (`project_id`), and authored records by member id

## Related

| Doc | Role |
|-----|------|
| [`concepts/model.md`](../concepts/model.md) | Concepts, rules, what Plat5 won't do |
| [`reference/api/identity.md`](api/identity.md) | Identity service API |
| [`reference/container-labels.md`](container-labels.md) | Namespace values |
| [`reference/gateway.md`](gateway.md) | Headers and service rules |
| [`reference/config/routes.md`](config/routes.md) | Route scopes, chain params and publish |
| [`reference/lists.md`](lists.md) | Identity list pagination (optional for business APIs) |
