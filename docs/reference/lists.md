# List pagination

How Plat5 **identity** public lists work. Business services behind the gateway may copy it.

Control-plane admin lists (`GET /revisions`) return the full set. So does identity's `GET /nodes/{node_id}/roles`: it reads the roles document, not a table ([`reference/api/identity.md`](api/identity.md#roles)).

## Query

| Param | |
|-------|--|
| `limit` | Optional. Default **50**, max **100**. Over-max is **clamped**, not 422. `< 1` or non-integer → **422** `VALIDATION_ERROR` |
| `starting_after` | Optional. Exclusive cursor: the `id` of the last item on the previous page. Omit on the first page |

`starting_after` must have the collection's id form: a prefixed ULID for identity (`n_01J…`), a bare ULID for audit events. Invalid → **422**. A well-formed id that isn't in the collection is not an error: the walk continues after that point in `id` order.

## Order

Always **`id` ascending**. Ids are ULIDs (time-sortable) behind a type prefix.

## Body

```json
{
  "nodes": [ ... ],
  "has_more": false
}
```

| Field | |
|-------|--|
| named collection | Plural resource key: `nodes`, `members`, `bindings`, `access`, `service_accounts`, `keys`, `invites`. `access` entries are keyed by binding id; `service_accounts` by member id |
| `has_more` | `true` if another page exists. Always present |

When `has_more` is `true`, pass the last item's `id` as `starting_after`.

## Filtered lists

Node lists through the gateway return only what the caller can read ([access](../concepts/access.md#lists)). A page can come back with fewer than `limit` items while `has_more` is `true`; keep walking until it's `false`.

## Audit events

Audit's reads take the same `limit` and `starting_after`, but run `id` **descending** (newest first). See [`reference/api/audit.md`](api/audit.md#reading).

## Lifecycle events

Identity's internal feed uses `starting_after` with a decimal sequence id, ascending ([`concepts/lifecycle.md`](../concepts/lifecycle.md#lifecycle-events)).
