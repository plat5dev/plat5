# Routes

Each service's routes are one routes file, listed in `plat5.yml`'s `services` ([config](../../concepts/config.md#the-config)). A route names its scope (a kind, or `public`), the labels it needs, and the upstream path the gateway writes. How the gateway uses them: [access](../../concepts/access.md#how-a-request-is-decided), [`reference/gateway.md`](../gateway.md).

## Config format

```yaml
# routes/e10s.yml
e10s:
  url: http://e10s:3000
  rate_limits:
    evals-project: { requests: 100, window_seconds: 1, count_at: project }
    evals-org:     { requests: 300, window_seconds: 1, count_at: organization }
  routes:
    - path: /orgs/{organization}/projects/{project}/subscribers/{subscriber_id}/entitlements/check
      scope: project
      upstream: /projects/{subject.project_id}/subscribers/{path.subscriber_id}/entitlements/check
      methods:
        POST: { required_labels: [check], rate_limit: [evals-project, evals-org], audit: false }
    - path: /orgs/{organization}/projects/{project}/plans/{key}/versions
      scope: project
      upstream: /projects/{subject.project_id}/plans/{path.key}/versions
      methods:
        GET:  { required_labels: [project:read] }
        POST: { required_labels: [catalog:write] }
    - path: /me/notification-settings
      scope: directory
      upstream: /people/{subject.member_id}/notification-settings
      methods: [GET, PUT]
    - path: /health
      scope: public
      methods: [GET]
```

The versions route doesn't need the author in its path: the service reads `X-Plat5-Member-Id` ([`reference/headers.md`](../headers.md)). Rewriting `{subject.member_id}` into the path is there for services that prefer it.

### Service fields

<!-- generated from schema/v2 with types: routes.json#/$defs/service_name routes.json#/$defs/service/properties -->
| Field | Type | Rule |
|-------|------|------|
| service name | `string` | The file's one top-level key: 1–128 of `[A-Za-z0-9._-]`. A rate-limit bucket prefix and the `service` on every audit event |
| `url` | `string` | Upstream, exactly `http://host:port` (explicit port, no path, no query), from the **gateway's** network view. `https://` is **422**: TLS upstreams aren't supported yet. Credentials in the URL, a port outside 1–65535, or any other form is **422** |
| `rate_limits` | `map<string, policy>?` | Named policies this service's routes may reference ([below](#rate_limit)) |
| `timeouts` | `{connect_ms, read_ms}?` | Upstream timeouts in ms, each 1–600000. One left out uses the gateway's `UPSTREAM_CONNECT_TIMEOUT_MS` / `UPSTREAM_READ_TIMEOUT_MS` |
| `route_prefix` | `string?` | Joined to every route `path` at apply. Not applied to `upstream` |
| `routes` | `array<route>` | At least one |
<!-- end generated -->

### Route fields

<!-- generated from schema/v2 with types: routes.json#/$defs/route/properties -->
| Field | Type | Rule |
|-------|------|------|
| `path` | `string` | Match path. Holds one param per kind in its scope's chain, named exactly as the kind ([below](#scope-and-path)) |
| `scope` | `string` | Required. A kind name, or `public`. A route without `scope` is **422**, so nothing becomes public by omission |
| `upstream` | `string?` | Absolute path template. Omitted means proxy `path` unchanged, which a path with chain params can't do: slugs never reach a service |
| `methods` | `array<string> \| map<string, null \| method config>` | List form: allowed methods, with route-level `required_labels` / `rate_limit` / `audit`. Map form: per-verb config, with no route-level `required_labels`, `rate_limit` or `audit`. Do not mix them (**422**) |
| `required_labels` | `array<string>?` | Required on every method of a route scoped to a kind other than the sign-in kind. `[]` is allowed and means any member of the tree. Omitted or `[]` on `public` and sign-in routes; anything else there is **422** |
| `rate_limit` | `false \| string \| array<string> \| {requests, window_seconds}?` | Omitted inherits the gateway fallback |
| `audit` | `bool?` | Omitted: the method default. Not on `public` routes |
| `resolve_deleted` | `bool?` | Default `false`. `true` lets the last node in the chain be a deleted (not purged) node. Its bindings are kept until purge and still count here, as Google Cloud keeps a deleted project's policy so it can be undeleted. For identity's restore route; see [`reference/api/internal.md`](../api/internal.md#admit). Only on routes scoped to a kind other than the sign-in kind |
<!-- end generated -->

Unknown fields are **422**.

### `methods`

**Flat list.** Route-level `required_labels` / `rate_limit` / `audit` apply to every method in the list.

```yaml
- path: /orgs/{organization}/projects/{project}/features
  scope: project
  upstream: /projects/{subject.project_id}/features
  methods: [GET, POST]
  required_labels: [catalog:write]
```

**Nested map.** Each key is an uppercase verb (`GET`, `POST`, `PUT`, `PATCH`, `DELETE`, `HEAD`, `OPTIONS`). The body sets `required_labels`, `rate_limit` and `audit` for that verb only. An empty methods map is **422**.

Nested maps are an apply-time convenience. The control plane expands each verb into its own stored row (same `path` and `upstream`, `methods: [THAT_VERB]`). After expand, stored `methods` is always a string array. A duplicate `path` + method after expand is **422**.

## Scope and path

| Scope | Path params for the chain | Subject fields | Labels |
|-------|--------------------------|----------------|--------|
| `public` | None | None | Omitted or `[]` |
| A kind, e.g. `project` | One per kind from the root down: `{organization}`, then `{project}` | `{kind}_id` for the node and each ancestor; `member_id`: the caller in that tree | Required |
| The sign-in kind, e.g. `directory` | None: the sign-in node has no slug | `member_id`: the person's directory member, their user id | Omitted or `[]` |

- **Chain params.** A kind's param is named exactly as the kind, and the chain's params appear in order, root first. A path for a kind with several allowed parents picks one chain per route: `/orgs/{organization}/projects/{project}` and `/orgs/{organization}/folders/{folder}/projects/{project}` are two routes. A param named after a kind outside the chain is **422**.
- **Values.** Each chain param takes a slug or a node id. The gateway resolves it under the previous node ([`concepts/access.md`](../../concepts/access.md#how-a-request-is-decided)).
- **Other params** are free: letters, digits and `_`, not starting with a digit, at most 64 characters. They can't be a kind name.
- **Literal segments** are free: `/orgs/{organization}/projects/{project}/…` uses each kind's `segment`, but nothing requires it.

On a sign-in route, `[]` means any signed-in person. Service accounts have no directory member, so their keys are **401** there.

## `upstream`

`path` is the match. `upstream` is an absolute template the gateway fills at request time; the control plane stores it with placeholders.

| Placeholder | Value |
|-------------|-------|
| `{subject.<kind>_id}` | The id of that kind's node in the chain, e.g. `{subject.organization_id}`, `{subject.project_id}` |
| `{subject.member_id}` | The caller's member id in this tree, or their directory member on a sign-in route |
| `{path.<param>}` | A non-chain path param, as matched |

Apply-time **422**:

- `subject.*` names a field the scope doesn't provide.
- `path.*` names a chain param (slugs never reach a service) or a param not in the expanded `path`.
- A bare `{foo}` in `upstream`. The namespace is the point.
- `upstream` is not an absolute path (starts with `/`, no `?` or `#`).
- `upstream` omitted on a path with chain params.

Substituted values are one path segment. A `{path.*}` value with `/`, `?` or `#` is **400**. A subject id that isn't one segment is **500**. The query string is preserved.

## `required_labels`

Labels follow role hygiene: `[a-z0-9:._-]+`, max 64 chars, max 32 per route, unique. Labels are opaque: `catalog:write` does not imply `project:read`.

After the rate limit, the caller's labels on the node (the union of the roles of its bindings on the node and its ancestors) must hold `*` or share one label with the route, or the gateway answers **403** "permission denied, or it may not exist". `[a, b]` means a or b. `[]` admits any member of the tree, guests included. A route can't require `*` itself. Roles and bindings: [`concepts/access.md`](../../concepts/access.md).

## `rate_limit`

Applies to every admitted request. Counters live in **Valkey**; replicas share one budget. `VALKEY_URL` is required to boot. A Valkey error or timeout on a limited request is **503** `SERVICE_UNAVAILABLE`.

Fixed window: the first increment opens the window; the key's TTL is `window_seconds`. Restarting Valkey resets open windows.

| YAML | Effect |
|------|--------|
| omitted | The gateway fallback, `RATE_LIMIT_REQUESTS` / `RATE_LIMIT_WINDOW_SECONDS`, counted at the route's own node. `0` requests = unlimited |
| `false` | Unlimited for this route or verb |
| `{requests, window_seconds}` | Its own bucket for this route + method, counted at the route's own node. Both > 0 |
| a policy name | That policy, defined on this service |
| a list of names | Every policy must pass. `X-RateLimit-*` report the tightest |

### Named policies (`rate_limits`)

<!-- generated from schema/v2: routes.json#/$defs/policy_name routes.json#/$defs/policy/properties -->
| Field | Rule |
|-------|------|
| policy name | `[a-z0-9:._-]+`, max 64. Unique in the service |
| `requests` | Required. Greater than 0 |
| `window_seconds` | Required. Greater than 0 |
| `count_at` | Optional. The kind whose node holds the bucket: the route's kind or an ancestor. Omitted: the route's own kind. A policy with `count_at` can't be used on a `public` route, and on a sign-in route only with the sign-in kind |
| `shared` | Optional bool. `true`: the same bucket across services that declare the name |
<!-- end generated -->

### Buckets

| Policy | Bucket |
|--------|--------|
| Inline object, or the fallback | `{METHOD} {path} {node_id}` |
| Name, not shared | `{service}:{name}:{node_id}` |
| Name, `shared: true` | `{name}:{node_id}` |

`node_id` is the node at the policy's `count_at`. On `public` routes the bucket is the client IP. On sign-in routes it's the caller's directory member id, since the sign-in node is one node for everyone.

A `count_at: project` bucket means a sandbox project can't spend a live project's budget; a `count_at: organization` bucket caps the org as a whole. A route can carry both.

**`shared: true`** is an opt-in join across services. Every service that uses the name declares the same entry (`requests`, `window_seconds`, `count_at`, `shared: true`). The config holds every service, so apply checks them together: the same shared name with different numbers, or one shared and one local, is **422**.

Exceed: **429** `RATE_LIMITED`, `Retry-After`, `details.retry_after_seconds`. Limited admitted routes set `X-RateLimit-Limit` / `Remaining` / `Reset` on success and on 429.

A separate failed-auth IP limiter (`RATE_LIMIT_AUTH_FAILURE_*`) covers unadmitted 401s, not-found 403s (a missing node, or a caller outside the tree), and unmatched 404s. It is not per-route.

The limiter runs before the label check, so a label-denied **403** counts against the route's limit.

## `audit`

Whether the gateway records this route + method in the audit log of the root whose tree it touched ([`concepts/audit.md`](../../concepts/audit.md)).

| YAML | Effect |
|------|--------|
| omitted | Method default: `POST`, `PUT`, `PATCH`, `DELETE` audited; `GET`, `HEAD` not |
| `true` | Audited. Use it on a read worth recording, such as one that returns secrets |
| `false` | Not audited. Use it on a high-volume write nobody needs in the log |

On `public` → **422**. `OPTIONS` is answered as a preflight and never audited. With audit off for the deployment, the gateway ignores the field.

## Route ownership

A route belongs to one service, compared by **shape**: each `{param}` matches any other `{param}`, a literal segment matches only the same literal. Two services in the config with the same method + shape reject the whole apply with **409** `ROUTE_CONFLICT`, listing each conflicting route and both services. Only the same method conflicts; literal vs param never conflicts; shapes inside one service never conflict.

## Validation

The [schema](../../concepts/config.md#schema) defines the shape, and the control plane checks it on every apply; the gateway checks each snapshot against it at load. Cross-document rules (scope, chain params, labels stated, `count_at`) are in [`concepts/config.md`](../../concepts/config.md#validation-at-apply). Shape rules:

- Paths are canonical and rejected, not rewritten: no trailing `/` (except `/`), no uppercase outside `{params}`. Applies after `route_prefix` is joined
- A path is visible ASCII, at most 1024 bytes, at most 32 params
- `methods` not empty; map keys are uppercase verbs; list and map not mixed
- `upstream` as [above](#upstream)
- `required_labels`, `rate_limit`, `rate_limits` and `audit` as above
- `timeouts`: `connect_ms` and `read_ms` each 1–600000
- Unknown fields **422**
- A snapshot that fails the gateway's validation is not loaded; the gateway keeps its current routes
