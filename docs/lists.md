# List pagination

How Plat5 **identity** public lists work.

Business services behind the gateway are **not** required to copy this. If you want the same client shape as identity, use this.

Route-registry admin lists (`GET /services`, revisions) are get-all. Do not paginate those.

## Query

| Param | |
|-------|--|
| `limit` | Optional. Default **50**, max **100**. Over-max is **clamped**, not 422. `< 1` or non-integer → **422** `VALIDATION_ERROR`. |
| `starting_after` | Optional. Exclusive cursor: the `id` of the last item on the previous page. Omit on the first page. |

No `offset`. No `page`. No `ending_before`.

`starting_after` must be a ULID (same form as resource `id`). Invalid → **422** `VALIDATION_ERROR` (fallback sentence). A well-formed ULID that is not in the collection is **not** an error — the walk continues after that point in `id` order.

## Order

Always **`id` ascending**. IDs are ULIDs (time-sortable). No other sort.

## Body

```json
{
  "organizations": [ ... ],
  "has_more": false
}
```

| Field | |
|-------|--|
| named collection | Plural resource key (`organizations`, `members`, `invites`, `service_accounts`, `keys`). Not `data`. |
| `has_more` | `true` if another page exists. Always present. |

When `has_more` is `true`, pass the last item’s `id` as `starting_after` on the following request. Do not send `starting_after` when `has_more` is `false`.

No cursor field in the body. No `total`. `limit` is not echoed.

## Identity lists

All of: list orgs, members, invites, service accounts, user API keys, member API keys.

## Stop

- Do not add `offset`
- Do not add a cursor field on the list object (`last`, `next`, `next_cursor`)
- Do not add bidirectional cursors
- Do not require this of customer APIs
- Do not paginate route-registry
