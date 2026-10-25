# Rate limits

Named policies in `plat5.yml`, shared by every route that names them. How the gateway applies them: [`reference/gateway.md`](../gateway.md#rate-limits).

```yaml
# in plat5.yml
rate_limits:
  default:       { requests: 60, window_seconds: 60 }
  evals-project: { requests: 100, window_seconds: 1, count_at: project }
  evals-org:     { requests: 300, window_seconds: 1, count_at: organization }
```

```yaml
# a route in services/e10s.yml
- path: /subscribers/{subscriber_id}/entitlements/check
  methods:
    POST: { required_permissions: [check], rate_limit: [evals-project, evals-org] }
```

<!-- generated from schema: rate_limits.json#/properties/rate_limits rate_limits.json#/$defs/policy_name rate_limits.json#/$defs/policy/properties -->
| Field | Rule |
|-------|------|
| `rate_limits` | Map of policy name → policy |
| policy name | `[a-z0-9:._-]+`, max 64. `default` is required, and is every route's that names none |
| `requests` | Required. Greater than 0 |
| `window_seconds` | Required. Greater than 0 |
| `count_at` | Optional. A kind: everyone at that kind's node shares one budget. Omitted: each caller has their own budget at the route's node |
<!-- end generated -->

## On a route

Every config has a `default` policy, without `count_at`: it applies to routes in every scope, and a `user` or `public` route has no node to count at (**422** otherwise).

A route's `rate_limit` is a list of policy names. Omitted, the route uses `default`. A route that should be unlimited says `rate_limit: false`. Changing `default` is an apply like any other.

Every route that names a policy shares its bucket: two routes, in one service or two, share a limit by naming the same policy. Separate budgets with the same numbers are two policies.

All of a route's policies are checked at once and counted only if every one passes, so a request one policy refuses spends nothing from the others. `X-RateLimit-*` report the tightest: the policy with the fewest requests remaining. A **429** carries the longest `Retry-After` among the policies exceeded.

## Buckets

| Policy | Route scope | One bucket per |
|--------|-------------|----------------|
| No `count_at` | A kind | Policy, the route's node, and the caller's member: each caller has their own budget at that node, as Google Cloud quotas are per user per project |
| `count_at: <kind>` | A kind | Policy and the node of that kind in the chain: everyone in it shares one budget |
| No `count_at` | `user` | Policy and the caller's user id |
| No `count_at` | `public` | Policy and the client IP |

`count_at` must be the route's kind or an ancestor, and only on routes in a kind scope (**422** otherwise). A `count_at: project` bucket means a sandbox project can't spend a live project's budget; a `count_at: organization` bucket caps the org as a whole. A route can carry both.

Fixed window: the first increment opens the window; the key's TTL is `window_seconds`. Counters live in Valkey, under a fixed Plat5 key prefix, so a Valkey shared with other software is safe.
