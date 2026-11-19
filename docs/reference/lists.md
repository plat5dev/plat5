# List pagination

Plat5's **identity** and **audit** APIs paginate lists this way. Your services can follow the same cursor pattern so clients have one pagination shape across every list endpoint.

Small fixed lists (`GET /revisions`, `GET /nodes/{node_id}/roles`) return the full set without pagination.

## Query

| Param | |
|-------|--|
| `limit` | Optional. Default **50**, max **100**. Over-max is **clamped**, not 422. `< 1` or non-integer → **422** `VALIDATION_ERROR` |
| `starting_after` | Optional. Exclusive cursor: the `id` of the last item on the previous page (a lowercase ULID). Omit on the first page |

## Order

Identity lists run **`id` ascending** (creation order for ULIDs). Audit event lists run **`id` descending** (newest first; see [`reference/api/audit.md`](api/audit.md#reading)).

## Body

```json
{
  "nodes": [ ... ],
  "has_more": false
}
```

| Field | |
|-------|--|
| named collection | Plural resource key (`nodes`, `members`, `users`, `bindings`, `service_accounts`, `keys`, `invites`, `audit_events`) |
| `has_more` | `true` if another page exists. Always present |

When `has_more` is `true`, pass the last item's `id` as `starting_after`.

## Filtered lists

Node lists through the gateway return only what the caller can see ([access](../concepts/access.md#lists)). `?status=deleted` lists deleted nodes instead of live ones. A filtered page can come back with fewer than `limit` items while `has_more` is `true`; keep walking until it's `false`.

## Lifecycle log

Identity's internal lifecycle log pages with `after` (a decimal sequence id, ascending) and long-polls for new events ([internal APIs](api/internal.md#lifecycle-events)).
