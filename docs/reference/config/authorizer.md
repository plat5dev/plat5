# Authorizer

Optional external **OpenID AuthZEN 1.0** policy decision point (PDP) such as Topaz, Cerbos, OpenFGA (with AuthZEN adapter), or a custom PDP.

Authorization in Plat5 is **all or nothing**: a deployment either uses Plat5's built-in RBAC (`roles` + `creator_role`) **or** delegates all access decisions (nodes and records) to a single top-level `authorizer`. Setting `authorizer` is mutually exclusive with `roles`, `creator_role`, `custom_roles`, and `permissions`. Plat5 still handles authentication, slug-to-ULID resolution, membership, org security policies, credential scopes, rate limiting, and audit.

```yaml
authorizer:
  url: http://topaz:9393
  timeout: 2s
  cache_ttl: 5s
  negative_cache_ttl: 2s
  token_env: TOPAZ_API_KEY
```

<!-- generated from schema: authorizer.json#/properties/authorizer authorizer.json#/$defs/authorizer/properties -->
| Field | Rule |
|-------|------|
| `authorizer` | External OpenID AuthZEN 1.0 PDP configuration. Mutually exclusive with `roles`, `creator_role`, `custom_roles`, and `permissions` |
| `url` | Base URL of an OpenID AuthZEN 1.0 PDP (`http://` or `https://`, optional path prefix without trailing slash, no credentials or query). Plat5 appends `/access/v1/evaluation` and `/access/v1/evaluations` |
| `timeout` | HTTP timeout for each AuthZEN request, as a duration (`2s`). Default `2s` |
| `cache_ttl` | How long the gateway caches positive (`decision: true`) answers from the authorizer (`0s` disables caching). Default `0s` |
| `negative_cache_ttl` | How long the gateway caches negative (`decision: false`) answers from the authorizer (`0s` disables caching). Default `0s` |
| `token_env` | Optional environment variable name on the gateway and identity holding a bearer token sent as `Authorization: Bearer <value>` to the PDP. Never put plaintext secrets in config |
<!-- end generated -->

## Record-level resources

By default, the gateway checks every kind-scoped route against its scope node (`{ "type": "<scope_kind>", "id": "<leaf_node_id>" }`) using the route's `permissions` as AuthZEN action names. To check a specific record inside a node, set `resource: { type, id }` on the route or verb:

<!-- generated from schema: authorizer.json#/$defs/resource/properties -->
| Field | Rule |
|-------|------|
| `type` | AuthZEN `resource.type` name (e.g. `document`) |
| `id` | AuthZEN `resource.id` template (e.g. `{doc_id}` or `{project.id}:{doc_id}`) |
<!-- end generated -->

```yaml
- path: /documents/{doc_id}
  resource: { type: document, id: "{project.id}:{doc_id}" }
  methods:
    GET:    { permissions: docs:read }
    PATCH:  { permissions: docs:write }
    DELETE: { permissions: docs:delete }
```

## Wire contract

Plat5 calls `POST {url}/access/v1/evaluation` for a single action and `POST {url}/access/v1/evaluations` for multi-permission OR routes (`evaluations_semantic: "permit_on_first_permit"`), `POST /access/routes` UI checks, and identity node-list filtering (`evaluations_semantic: "execute_all"`).

- **Subject:** `{ "type": "member", "id": "<member_id>", "properties": { "principal": "person" | "service_account", "root_id": "<root_id>" } }`.
- **Resource:** Defaults to `{ "type": "<scope_kind>", "id": "<leaf_node_id>" }` (or the route's `resource: { type, id }` template filled with resolved IDs and route params), with `properties` carrying `root_id`, `<kind>_id` for each chain node, and `nodes`.
- **Action:** Each permission in the route's `permissions` (for kind list filtering in identity, the kind's `list_permission`). Gateway requests also include `action.properties: { "method": "<HTTP_METHOD>", "route": "<route_template>" }`.
- **Context:** Gateway requests include `client_ip`, `request_id`, and matched route `params`.
- **Fail closed:** Timeouts, transport errors, HTTP redirects, non-2xx responses, or invalid JSON return **503** `SERVICE_UNAVAILABLE`. A `decision: false` takes the same uniform **403** path (and failed-auth IP limiter) as built-in RBAC.
- **Credential scopes:** If a key or JWT is scope-narrowed, only actions permitted by the credential's scopes can succeed; an external PDP can never widen a narrowed credential.
- **Caching:** `cache_ttl` and `negative_cache_ttl` default to `0s` (immediate consistency, no cache). Set short TTLs (for example `2s`–`5s`) on high-traffic read endpoints when a few seconds of staleness is acceptable.
