# Naming Conventions

Formats and terminology for labels, routes, metrics, and error codes across Plat5.

## Project Terminology

- **Plat5** — Brand and main product: gateway, API keys, organizations, route registry, contracts (platform runtime).
- **Plat5 Auth** — Optional reference OIDC IdP (separate product).
- **Plat5 Cloud** — Hosted multi-tenant control plane (separate product, deferred).
- **Service** — Any unit of code that runs with Plat5 (e.g. `gateway`, `api-keys`, `widgets`).
- **Platform service** — Plat5 runtime units: `gateway`, `route-registry`, `api-keys`, `organizations`.
- **Business service** — Use-case service behind Plat5: domain APIs.
- **IdP** — External identity provider (JWT issuer). Not part of Plat5 (optional Plat5 Auth is a separate product).

### Scope of Plat5

Plat5 owns **opaque user ids** (as strings from the gateway), API keys, organizations, memberships, route registry, and the user-facing gateway. It does **not** own a user directory, login UI, or credential store — that is the IdP. It does not own admin/employee planes, operator RBAC, hosted multi-tenant control planes, or application resource authorization (projects, docs, etc.). See [`identity-boundary.md`](identity-boundary.md).

### Identity domain nouns

| Concept | Formal name | Notes |
|---------|-------------|--------|
| Global person | **User** | `user_id` (opaque string from IdP via gateway) |
| Isolation boundary users join | **Organization** | Not “tenant” — **tenant** means a Plat5 Cloud customer account (out of scope for Plat5 runtime) |
| User ↔ organization link | **Membership** | Lifecycle + org-admin fields live on this row |
| Membership role | Organizations service domain | Org admin APIs only — not gateway identity or org-scope headers |

**Rejected service names:** `org-service`, `orgs`, `tenants`, `tenancy`, `memberships` (alone), `identity` (as a single service name for the whole product), `rbac`, `authz`, `accounts`.

The organizations service is **`organizations`** (`service.name: organizations`).

## Service Names

- Format: `kebab-case`
- Examples: `api-keys`, `gateway`, `organizations`
- Match the service directory name
- Used in `service.name` container label and OTel resource attribute

## Service Namespaces

Full table and labels: [`container-labels.md`](container-labels.md). Identity backends use **`identity`**; gateway stays **`edge`**.

| Service | `service.namespace` |
|---------|---------------------|
| `api-keys`, `organizations` | `identity` |
| `gateway`, `route-registry` | `edge` |
| Business APIs | `api` (etc.) |

When changing labels: update compose labels and `OTEL_SERVICE_NAMESPACE` together.

## HTTP Routes

- `kebab-case` path segments: `/api/user-profiles`
- Prefix by surface — **no path version segment**:
  - Authenticated API: `/api/...`
  - Public API: `/public/...`
  - Internal (private network, not on the gateway): `/internal/...`
- Do **not** use `/api/v1` or `/public/v1`. Version resources or media types later if needed.
- Resource names are plural nouns: `/widgets`, `/users`, `/organizations`
- Actions use HTTP methods, not verbs in paths:
  - `POST /api/widgets` — create
  - `GET /api/widgets/{id}` — read
  - `PUT /api/widgets/{id}` — update
  - `DELETE /api/widgets/{id}` — delete

### Path patterns (identity)

| Surface | Pattern | Route scope |
|---------|---------|-------------|
| Organizations service (authority) | `/api/organizations`, `/api/organizations/{organization_id}/...` | **`user`** only |
| Business APIs under an org | e.g. `/api/organizations/{organization_id}/projects` | **`organization`** |
| Internal control (not on gateway) | `/internal/keys/validate`, `/internal/memberships/resolve` | private / `INTERNAL_PORT` |

Scopes and headers: [`gateway-contract.md`](gateway-contract.md).

## Gateway identity headers

Headers are **scope-specific**. Gateway strips client-supplied identity headers, then injects only what the route scope allows. Full contract: [`gateway-contract.md`](gateway-contract.md).

| Scope | Identity headers injected |
|-------|---------------------------|
| `public` | none |
| `user` | `X-User-Id` only |
| `organization` | `X-Organization-Id`, `X-Membership-Id` only — **not** `X-User-Id` |

Membership **role** is not a gateway header. Always (all scopes): `X-Request-ID`, `traceparent`.

## Log Fields

- Format: `snake_case`
- Common: `request_id`, `user_id`, `organization_id`, `membership_id`, `duration_ms`, `error_kind`
- Service-specific fields should be namespaced: `auth_provider`, `db_operation`

## Metric Names

- Format: `snake_case`
- Structure: `<domain>_<entity>_<unit>`
- Examples: `http_requests_total`, `http_request_duration_seconds`, `db_operations_total`, `process_resident_memory_bytes`

## Error Codes

- Format: `UPPER_SNAKE_CASE`
- Full list: [`api-errors.md`](api-errors.md)

## Environment Variables

- Format: `UPPER_SNAKE_CASE`
- Group by prefix when related: `OTEL_SERVICE_NAME`, `OTEL_EXPORTER_OTLP_ENDPOINT`
- Examples: `PORT`, `DATABASE_URL`, `DEPLOYMENT_ENV`

## Database

- Table names: `snake_case`, plural: `api_keys`, `organizations`, `organization_memberships`
- Column names: `snake_case`: `created_at`, `updated_at`, `user_id`, `organization_id`, `membership_id`
- Foreign key columns: `<entity>_id`
- New identity row IDs: **ULID** strings (`organization_id`, `membership_id`)

## Related

| Doc | Role |
|-----|------|
| [`identity-boundary.md`](identity-boundary.md) | Authn vs org context vs resource authz |
| [`container-labels.md`](container-labels.md) | Namespace values |
| [`gateway-contract.md`](gateway-contract.md) | Headers and service rules |
| [`routes.md`](routes.md) | Route scopes and publish |
