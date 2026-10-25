# Services

Each service is one file, listed in `plat5.yml`'s `services` ([config](../../concepts/config.md#the-config)). The file's one top-level key is the service's name; under it sit the service's own settings and its routes, grouped by scope. How the gateway uses them: [access](../../concepts/access.md#how-a-request-is-decided), [`reference/gateway.md`](../gateway.md).

`identity` and `audit` are reserved names: their files take `resources` instead of `scopes` ([resources](resources.md)).

## Format

```yaml
# services/e10s.yml
e10s:
  url: http://e10s:3000
  timeouts: { connect: 2s, read: 30s }
  scopes:
    project:
      upstream_prefix: /projects/{project.id}
      routes:
        - path: /subscribers/{subscriber_id}/entitlements/check
          methods:
            POST: { required_permissions: [check], rate_limit: [evals-project, evals-org], audit: false }
        - path: /plans/{plan_id}/versions
          required_permissions: [project:read]
          methods:
            GET:
            POST: { required_permissions: [catalog:write] }
    user:
      routes:
        - path: /user/notification-settings
          upstream: /users/{user.id}/notification-settings
          methods: [GET, PUT]
    public:
      routes:
        - path: /health
          methods: [GET]
```

With the kinds in [kinds](kinds.md), the versions route is `GET` and `POST /orgs/{organization}/projects/{project}/plans/{plan_id}/versions`, forwarded to `/projects/{project.id}/plans/{plan_id}/versions`. It doesn't need the author in its path: the service reads `X-Plat5-Member-Id` ([`reference/headers.md`](../headers.md)).

### Service fields

<!-- generated from schema with types: services.json#/$defs/service_name services.json#/$defs/service/properties -->
| Field | Type | Rule |
|-------|------|------|
| service name | `string` | The file's one top-level key: 1–128 of `[A-Za-z0-9._-]`. `identity` and `audit` take [resources](resources.md). The `service` on every audit event |
| `url` | `string` | Upstream, exactly `http://host:port` (explicit port, no path, no query), from the **gateway's** network view. `https://` is **422**: TLS upstreams aren't supported. Credentials in the URL, a port outside 1–65535, or any other form is **422** |
| `timeouts` | `{connect, read}?` | Upstream timeouts, each a duration. One left out uses `defaults.timeouts` ([settings](settings.md)) |
| `max_body_bytes` | `integer?` | The largest request body forwarded to this service. Larger is **413**. Omitted uses `defaults.max_body_bytes` |
| `scopes` | `map<scope, scope block>` | One block per scope: a kind name, `user` or `public`. At least one route in all |
<!-- end generated -->

### Scope fields

<!-- generated from schema with types: services.json#/$defs/scope/properties -->
| Field | Type | Rule |
|-------|------|------|
| `upstream_prefix` | `string?` | Prepended to each route's path to make its default upstream. Placeholders with a dot only ([below](#upstream)) |
| `routes` | `array<route>` | At least one |
<!-- end generated -->

### Route fields

<!-- generated from schema with types: services.json#/$defs/route/properties -->
| Field | Type | Rule |
|-------|------|------|
| `path` | `string` | In a kind scope, relative to the node: `/` is the node itself. In `user` and `public`, absolute |
| `upstream` | `string?` | A full upstream template, replacing `upstream_prefix` plus `path` |
| `methods` | `array<string> \| map<string, null \| method>` | The verbs: `GET`, `POST`, `PUT`, `PATCH`, `DELETE`, `HEAD`. A list is a map with no overrides. In a map, each verb's `required_permissions`, `rate_limit` and `audit` override the route's |
| `required_permissions` | `array<string> \| "any_member"?` | Any one of these permissions admits. `any_member` admits any member of the tree, guests included. Required for every verb in a kind scope, from the route or the verb. Not in `user` or `public` |
| `rate_limit` | `array<string> \| false?` | Policies from `rate_limits` ([rate limits](rate-limits.md)). Omitted: `[default]`. `false`: none |
| `audit` | `bool?` | Omitted: the method default. Not in `public` |
| `allow_deleted` | `bool?` | Default `false`. `true` lets the route's node be deleted (not purged); its own bindings are kept until purge and still count here, so whoever managed the node can restore it. For identity's restore route ([`reference/api/internal.md`](../api/internal.md#admit)). Kind scopes only |
<!-- end generated -->

Unknown fields are **422**.

## Scopes and paths

| Scope | Public path | Placeholders | Permissions |
|-------|-------------|--------------|-------------|
| A kind, e.g. `project` | Each kind's `path` piece, root first, then the route's `path`: `/orgs/{organization}/projects/{project}` + `/plans/{plan_id}/versions` | `{<kind>.id}` for the node and each ancestor; `{member.id}`, the caller in that tree | Required |
| `user` | The route's `path`, as written | `{user.id}`, the caller | None |
| `public` | The route's `path`, as written | None | None |

- **Kind params.** The kind pieces' params (`{organization}`, `{project}`) take a slug or a node id; the gateway resolves each under the previous node ([`concepts/access.md`](../../concepts/access.md#how-a-request-is-decided)). They never reach a service, so they never appear in an upstream.
- **The route's own params** are free: letters, digits and `_`, not starting with a digit, at most 64 characters. They can't be a kind name. Each param is a whole segment: `/files/{name}.json` is **422**.
- **A child kind's collection.** A route in a kind's scope may be exactly a child kind's collection (`/projects` in `organization`), but nothing under it (`/projects/archived` is **422**): a project named `archived` would be shadowed by the literal. Use `/archived-projects`, or `/projects?status=archived`.

A user route is for the person's own data, keyed by the user id. Service accounts can't call user routes: their keys are **401** there.

## `upstream`

A route's upstream defaults to the scope's `upstream_prefix` plus the route's `path` (`/` adds nothing), with the route's own params sent as matched. `upstream` on a route replaces it with a full template. Both are absolute paths the gateway fills at request time.

| Placeholder | Value | In |
|-------------|-------|----|
| `{<kind>.id}` | The id of that kind's node in the chain: `{organization.id}`, `{project.id}` | Kind scopes |
| `{member.id}` | The caller's member id in this tree | Kind scopes |
| `{user.id}` | The caller's user id | `user` |
| `{<param>}` | One of the route's own params, as matched | `upstream` |

Only `.id` exists, so slugs never reach a service. A dot always means a resolved value, and params can't be kind names, so nothing is ambiguous. The query string is preserved.

Apply-time **422**:

- A placeholder the scope doesn't provide: a kind outside the chain, `{member.id}` outside a kind scope, `{user.id}` outside `user`.
- A bare `{param}` that isn't one of the route's own params, or one in `upstream_prefix`.
- An upstream that isn't an absolute path (starts with `/`, no `?` or `#`).

## `required_permissions`

Permissions follow role hygiene: `[a-z0-9:._-]+`, max 64 chars, max 32 per route, unique. Permissions are opaque: `catalog:write` does not imply `project:read`.

After the rate limit, the caller's permissions on the node (the union of the roles of its bindings on the node and its ancestors) must hold `*` or one of the route's, or the gateway answers **403** "permission denied, or it may not exist". `[a, b]` means a or b. `any_member` admits any member of the tree, guests included: to keep guests out of an org-level route, require a permission at the org, such as `organization:read`, since a guest's bindings sit only below the root. A route can't require `*` itself. `[]` is **422**: write `any_member`. Roles and bindings: [`concepts/access.md`](../../concepts/access.md).

A route permission that no role grants is legal: only `*` grants it, which is deliberate for routes meant for admins alone.

## `audit`

Whether the gateway records this route and method in the audit log ([`concepts/audit.md`](../../concepts/audit.md)).

| YAML | Effect |
|------|--------|
| omitted | Method default: `POST`, `PUT`, `PATCH`, `DELETE` audited; `GET`, `HEAD` not |
| `true` | Audited. Use it on a read worth recording, such as one that returns secrets |
| `false` | Not audited. Use it on a high-volume write nobody needs in the log |

In `public` → **422**. With audit off for the deployment, the gateway ignores the field.

## Matching

The gateway matches the expanded public paths of every service. Two routes with the same method and the same **shape** (each param matches any other param, a literal only the same literal) are **422** at apply, naming both field paths, within one service or across services. When a request matches several shapes, a literal segment beats a param, segment by segment from the left: `/orgs/{organization}/projects` beats `/orgs/{organization}/{thing}`.

`OPTIONS` isn't a route method (**422**): the gateway answers every `OPTIONS` as a CORS preflight.

## Validation

The [schema](../../concepts/config.md#schema) defines the shape, and the control plane checks it on every apply, with the [rules](../../concepts/config.md#validation-at-apply) that tie the documents together. Route rules:

- Paths are canonical and refused, not rewritten: no trailing `/` (except `/`), no uppercase outside `{params}`, no empty segment
- A path is visible ASCII, at most 1024 bytes, at most 32 params
- `methods` not empty; map keys are uppercase verbs other than `OPTIONS`
- Placeholders and upstreams as [above](#upstream)
- `required_permissions` stated for every verb in a kind scope; absent in `user` and `public`
- `allow_deleted` only in a kind scope; `audit` not in `public`
- Every `rate_limit` name is a policy; a policy with `count_at` only in a kind scope, at that kind or an ancestor. `default` has no `count_at`
- Timeouts are durations; `max_body_bytes` at least 1
