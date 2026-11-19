# Rate limits

Optional named policies in the control-plane config, shared by every route that names them. How the gateway enforces them (and how to turn rate limiting off with `RATE_LIMIT_ENABLED=false`): [`reference/gateway.md`](../gateway.md#rate-limits).

```yaml
rate_limits:
  standard:      { requests: 60, window: 60s }
  evals-project: { requests: 100, window: 1s, count_at: project }
  evals-org:     { requests: 300, window: 1s, count_at: organization }

defaults:
  rate_limit: [standard]
```

```yaml
# a route in a service definition
- path: /subscribers/{subscriber_id}/entitlements/check
  methods:
    POST: { required_permissions: [check], rate_limit: [evals-project, evals-org] }
```

<!-- generated from schema: rate_limits.json#/properties/rate_limits rate_limits.json#/$defs/policy_name rate_limits.json#/$defs/policy/properties -->
| Field | Rule |
|-------|------|
| `rate_limits` | Map of policy name → policy |
| policy name | `[a-z0-9:._-]+`, max 64. No name is special |
| `requests` | Required. Greater than 0 |
| `window` | Required. Fixed window length as a duration (`60s`, `1m`) |
| `count_at` | Optional. A kind: everyone at that kind's node shares one budget. Omitted: each caller has their own budget at the route's node |
<!-- end generated -->

## On a route

A route's `rate_limit` is a list of policy names. Omitted, the route uses `defaults.rate_limit` if set ([settings](settings.md)), or is unlimited when `defaults.rate_limit` is omitted. A route that should be unlimited even when `defaults.rate_limit` is set says `rate_limit: false`.

Each policy `defaults.rate_limit` names must have no `count_at`: defaults apply to routes in every scope, and `user` or `public` routes have no node to count at (**422** otherwise).

Every route that names a policy shares its bucket: two routes, in one service or two, share a limit by naming the same policy. Separate budgets with the same numbers are two policies.

All of a route's policies are checked at once and counted only if every one passes, so a request one policy refuses spends nothing from the others. `X-RateLimit-*` report the tightest: the policy with the fewest requests remaining. A **429** carries the longest `Retry-After` among the policies exceeded.

## Buckets

| Policy | Route scope | One bucket per |
|--------|-------------|----------------|
| No `count_at` | A kind | Policy, the route's node, and the caller's member: each caller has their own budget at that node, so one busy caller can't spend another's |
| `count_at: <kind>` | A kind | Policy and the node of that kind in the chain: everyone in it shares one budget |
| No `count_at` | `user` | Policy and the caller's user id |
| No `count_at` | `public` | Policy and the client IP |
| Each policy `defaults.rate_limit` names | `POST /access/routes` | Policy and the credential (a key's hash, or a JWT's issuer and subject): [gateway](../gateway.md#rate-limits) |

`count_at` must be the route's kind or an ancestor, and only on routes in a kind scope (**422** otherwise). A `count_at: project` bucket means a sandbox project can't spend a live project's budget; a `count_at: organization` bucket caps the org as a whole. A route can carry both.

Fixed window: the first increment opens the window; the key's TTL is `window`. Counters live in Valkey under a fixed Plat5 key prefix, so a Valkey shared with other software is safe. When `RATE_LIMIT_ENABLED=false` on the gateway, `rate_limits` and route `rate_limit` fields are still accepted at apply and ignored at request time.
