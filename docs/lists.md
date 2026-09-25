# List pagination

How Plat5 **identity** public lists work.

Business services behind the gateway may copy this. Identity public lists use it.

Route-registry admin lists (`GET /services`, revisions) return the full set.

## Query

| Param | |
|-------|--|
| `limit` | Optional. Default **50**, max **100**. Over-max is **clamped**, not 422. `< 1` or non-integer → **422** `VALIDATION_ERROR`. |
| `starting_after` | Optional. Exclusive cursor: the `id` of the last item on the previous page. Omit on the first page. |

`starting_after` must be a ULID (same form as resource `id`). Invalid → **422** `VALIDATION_ERROR` (fallback sentence). A well-formed ULID that is not in the collection is **not** an error — the walk continues after that point in `id` order.

## Order

Always **`id` ascending**. IDs are ULIDs (time-sortable).

## Body

```json
{
  "organizations": [ ... ],
  "has_more": false
}
```

| Field | |
|-------|--|
| named collection | Plural resource key (`organizations`, `memberships`, `members`, `invites`, `service_accounts`, `keys`). |
| `has_more` | `true` if another page exists. Always present. |

When `has_more` is `true`, pass the last item’s `id` as `starting_after` on the following request.

## Identity lists

All of: list orgs, memberships, members, invites, service accounts, user API keys, member API keys.
