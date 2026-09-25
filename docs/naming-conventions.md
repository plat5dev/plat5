# Naming Conventions

Formats and terminology for labels, routes, metrics, and error codes across Plat5.

## Project Terminology

- **Plat5** — Brand and main product: gateway, identity, route registry, contracts (platform runtime).
- **Plat5 Auth** — Optional reference OIDC IdP (separate product).
- **Service** — Any unit of code that runs with Plat5 (e.g. `gateway`, `identity`, `widgets`).
- **Platform service** — Plat5 runtime units: `gateway`, `route-registry`, `identity`.
- **Business service** — Use-case service behind Plat5: domain APIs.
- **IdP** — External identity provider (JWT issuer). Not part of Plat5 (optional Plat5 Auth is a separate product).

### Scope of Plat5

Plat5 owns **opaque user ids** (as strings from the gateway), API keys, member sessions, organizations, members, service accounts, route registry, and the user-facing gateway. Login and the user directory are the IdP. Resource permissions are the service behind the gateway. See [`identity-boundary.md`](identity-boundary.md).

### Identity domain nouns

| Concept | Formal name | Notes |
|---------|-------------|--------|
| Global person | **User** | `user_id` (opaque string from IdP via gateway) |
| Isolation boundary | **Organization** | Do not call it tenant. |
| Org principal | **Member** | User *or* service account in an org; wire id `member_id` |
| Non-human org identity | **Service account** | Created under an organization; always has a member row |
| Credential | **API key** | User-scoped or member-scoped |
| Short-lived org credential | **Member session** | One active user member, one org. Not an API key. |

**Rejected service names:** `org-service`, `orgs`, `tenants`, `tenancy`, `memberships` (alone), `rbac`, `authz`, `accounts`, `api-keys` (as a standalone platform service).

The identity control-plane service is **`identity`** (`service.name: identity`).

## Service Names

- Format: `kebab-case`
- Examples: `identity`, `gateway`, `route-registry`
- Match the service directory name
- Used in `service.name` container label and OTel resource attribute

## Service Namespaces

Full table and labels: [`container-labels.md`](container-labels.md). Identity backend uses **`identity`**; gateway stays **`edge`**.

| Service | `service.namespace` |
|---------|---------------------|
| `identity` | `identity` |
| `gateway`, `route-registry` | `edge` |
| Business APIs | `api` (etc.) |

When changing labels: update compose labels and `OTEL_SERVICE_NAMESPACE` together.

## HTTP Routes

- `kebab-case` path segments: `/api/user-profiles`
- Prefix by surface — **no path version segment**:
  - Authenticated API: `/api/...` (identity is the exception: no `/api`; the first segment is the subject)
  - Public API: `/public/...`
  - Internal (private network, not on the gateway): `/internal/...`
- Resource names are plural nouns: `/widgets`, `/users`, `/organizations`, `/members`
- Identity list query/body: [`lists.md`](lists.md). Business services may copy it; they are not required to.
- Actions use HTTP methods, not verbs in paths:
  - `POST /api/widgets` — create
  - `GET /api/widgets/{id}` — read
  - `PUT /api/widgets/{id}` — update
  - `DELETE /api/widgets/{id}` — delete

### Path patterns (identity)

Identity has no `/api` prefix. The first path segment is the subject.

| Surface | Pattern |
|---------|---------|
| User | `/users/{user_id}/...` |
| Organization | `/organizations/{organization_id}/...` |
| Member | `/members/{member_id}/...` |
| Internal (not on the gateway) | `/internal/user-keys/validate`, `/internal/member-keys/validate`, `/internal/member-sessions/validate` |

Those are identity's own URLs. The gateway edge does not put the subject in the path.

| Edge | Scope | Upstream fills |
|------|-------|----------------|
| `/user/...` | `user` | `{subject.user_id}` |
| `/org/...` | `organization` | `{subject.organization_id}` |
| `/member/...` | `member` | `{subject.member_id}` (and `organization_id` if the template needs it) |

Business APIs stay under `/api/...`. A route that needs the subject sets `upstream`. The match path does not contain a param whose name is a subject field of that scope.

Scopes and subject fill: [`gateway-contract.md`](gateway-contract.md). Full identity API: [`identity.md`](identity.md).

## Subject

The route scope names the subject. `upstream` fills it. The client path does not. Full contract: [`gateway-contract.md`](gateway-contract.md).

| Scope | Subject fields |
|-------|----------------|
| `public` | none |
| `user` | `user_id` |
| `organization` | `organization_id` |
| `member` | `organization_id`, `member_id` |

Always (all scopes): `X-Request-ID`, `traceparent`.

## Log Fields

- Format: `snake_case`
- Common: `request_id`, `user_id`, `organization_id`, `member_id`, `duration_ms`, `error_kind`
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

- Table names: `snake_case`, plural: `user_api_keys`, `member_api_keys`, `organizations`, `members`, `service_accounts`
- Column names: `snake_case`: `created_at`, `updated_at`, `user_id`, `organization_id`, `member_id`, `service_account_id`
- Foreign key columns: `<entity>_id`
- New identity row IDs: **ULID** strings (`organization_id`, `member_id`, `service_account_id`, key ids)

## Related

| Doc | Role |
|-----|------|
| [`identity-boundary.md`](identity-boundary.md) | Authn vs org context vs resource authz |
| [`identity.md`](identity.md) | Identity service API |
| [`container-labels.md`](container-labels.md) | Namespace values |
| [`gateway-contract.md`](gateway-contract.md) | Headers and service rules |
| [`routes.md`](routes.md) | Route scopes and publish |
| [`lists.md`](lists.md) | Identity list pagination (optional for business APIs) |
