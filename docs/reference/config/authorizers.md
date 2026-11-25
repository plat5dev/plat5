# Authorizers

Optional external **OpenID AuthZEN 1.0** policy decision points (PDPs) such as Topaz, Cerbos, OpenFGA (with AuthZEN adapter), or a custom PDP. When configured, the gateway and identity delegate authorization decisions to the PDP while keeping Plat5's authentication, slug-to-ULID resolution, org security policies, credential scopes, rate limiting, and audit log intact.

```yaml
authorizers:
  topaz:
    url: http://topaz:8383
    timeout: 2s
    cache_ttl: 5s
    negative_cache_ttl: 2s
    token_env: TOPAZ_API_KEY
```

<!-- generated from schema: authorizers.json#/properties/authorizers authorizers.json#/$defs/authorizer_name authorizers.json#/$defs/authorizer/properties -->
| Field | Rule |
|-------|------|
| `authorizers` | Map of authorizer name → external AuthZEN 1.0 PDP configuration |
| authorizer name | Lowercase identifier, `[a-z][a-z0-9_-]*`, max 64 |
| `url` | Base URL of an OpenID AuthZEN 1.0 PDP (`http://` or `https://`, optional path prefix without trailing slash, no credentials or query). Plat5 appends `/access/v1/evaluation` and `/access/v1/evaluations` |
| `timeout` | HTTP timeout for each AuthZEN request, as a duration (`2s`). Default `2s` |
| `cache_ttl` | How long the gateway caches positive (`decision: true`) answers from this authorizer (`0s` disables caching). Default `0s` |
| `negative_cache_ttl` | How long the gateway caches negative (`decision: false`) answers from this authorizer (`0s` disables caching). Default `0s` |
| `token_env` | Optional environment variable name on the gateway and identity holding a bearer token sent as `Authorization: Bearer <value>` to the PDP. Never put plaintext secrets in config |
<!-- end generated -->

## Composition modes

An authorizer attaches to a kind, service, scope, route, or verb with one of three modes:

<!-- generated from schema: authorizers.json#/$defs/mode authorizers.json#/$defs/kind_authorizer authorizers.json#/$defs/route_authorizer authorizers.json#/$defs/resource/properties -->
| Field | Rule |
|-------|------|
| mode | How the external authorizer combines with Plat5's built-in RBAC: `replace` (default: PDP decides alone), `and` (Plat5 RBAC and PDP must both allow), or `or` (either Plat5 RBAC or PDP allows) |
| kind authorizer | External authorizer for this kind's `list_permission` filtering, identity `/access/v1/*` evaluation, and kind-scoped routes (unless overridden or opted out with `false`). Either an authorizer name (implies `mode: replace`) or `{ name, mode }` |
| route authorizer | External AuthZEN authorizer for a service, scope, route or verb (kind scopes only). Either an authorizer name (implies `mode: replace`), `false` to opt out of an inherited authorizer, or `{ name, mode?, resource?, action? }` |
| `type` | AuthZEN `resource.type` name (e.g. `document`) |
| `id` | AuthZEN `resource.id` template (e.g. `{doc_id}` or `{project.id}:{doc_id}`) |
<!-- end generated -->

- **`replace` (default):** Plat5 still checks credentials, org security policies, member suspension, and credential scopes; the external PDP alone decides whether the action is allowed.
- **`and`:** Plat5's built-in RBAC must allow first (short-circuiting on deny without calling the PDP), and the external PDP must also allow. Useful for record-level narrowing, ABAC rules, or explicit denies.
- **`or`:** Plat5's built-in RBAC is checked first (short-circuiting on allow so org admins bypass the PDP), otherwise falls through to the external PDP. Useful for additive per-document sharing.

## Inheritance on routes

On services, `authorizer` inherits from **kind → service → kind scope → route → verb**. A kind-level `authorizer` applies to that kind's routes that declare explicit `required_permissions` (never `any_member` routes such as child lists or `/membership`). Setting `authorizer: false` at a lower level opts that scope, route, or verb back out to Plat5's built-in RBAC. A service-level `authorizer` applies only to kind scopes (never `user` or `public`).

## Wire contract

Plat5 calls `POST {url}/access/v1/evaluation` for a single action and `POST {url}/access/v1/evaluations` for multi-permission OR routes (`evaluations_semantic: "permit_on_first_permit"`), `POST /access/routes` UI checks, and identity node-list filtering (`evaluations_semantic: "execute_all"`).

- **Subject:** `{ "type": "member", "id": "<member_id>", "properties": { "principal": "person" | "service_account", "root_id": "<root_id>" } }`. On `allow_public` routes without membership, `subject.type` is `"anonymous"` (`id: ""`) or `"user"` (`id: "<user_id>"`).
- **Resource:** Defaults to `{ "type": "<scope_kind>", "id": "<leaf_node_id>" }` (or the route's `resource: { type, id }` template filled with resolved IDs and route params), with `properties` carrying `root_id`, `<kind>_id` for each chain node, and `nodes`.
- **Action:** The route's `authorizer.action` override, or each permission in `required_permissions` (for kind list filtering, the kind's `list_permission`).
- **Context:** Includes `client_ip`, `request_id`, and matched route `params`.
- **Fail closed:** Timeouts, transport errors, non-2xx responses, or invalid JSON return **503** `SERVICE_UNAVAILABLE`. A `decision: false` takes the same uniform **403** path (and failed-auth IP limiter) as built-in RBAC.
- **Credential scopes:** If a key or JWT is scope-narrowed, only actions permitted by the credential's scopes can succeed; an external PDP can never widen a narrowed credential.
- **Caching:** Per-authorizer `cache_ttl` and `negative_cache_ttl` default to `0s` (immediate consistency, no cache). Set short TTLs (for example `2s`–`5s`) on high-traffic read endpoints when a few seconds of staleness is acceptable.
