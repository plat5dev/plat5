# List pagination

How Plat5 **identity** public lists work.

Business services behind the gateway are **not** required to copy this. If you want the same client shape as identity, use this.

Route-registry admin lists (`GET /v1/services`, revisions) are get-all. Do not paginate those.

## Query

| Param | |
|-------|--|
| `limit` | Optional. Default **50**, max **100**. Over-max is **clamped**, not 422. `< 1` or non-integer → **422** `VALIDATION_ERROR`. |
| `starting_after` | Optional. Exclusive cursor: `last` from the previous page. Omit on the first page. |

No `offset`. No `page`. No `ending_before`.

`starting_after` must be a ULID (same form as resource `id`). Invalid → **422** `VALIDATION_ERROR` (fallback sentence). A well-formed ULID that is not in the collection is **not** an error — the walk continues after that point in `id` order.

## Order

Always **`id` ascending**. IDs are ULIDs (time-sortable). No other sort.

## Body

```json
{
  "organizations": [ ... ],
  "last": "01HZX..."
}
```

| Field | |
|-------|--|
| named collection | Plural resource key (`organizations`, `members`, `invites`, `service_accounts`, `keys`). Not `data`. |
| `last` | ULID of the last item on this page, or `null`. Always present. `null` means last page. |

Pass `last` as `starting_after` on the following request. Do not send `starting_after` when `last` is `null`.

No `has_more`. No `total`. `limit` is not echoed.

## Identity lists

All of: list orgs, members, invites, service accounts, user API keys, member API keys.

## Stop

- Do not add `offset` or `has_more`
- Do not add bidirectional cursors
- Do not require this of customer APIs
- Do not paginate route-registry
