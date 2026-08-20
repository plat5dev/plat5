# Error copy — Stripe-style `message`

Copy appendix for [`api-errors.md`](api-errors.md).

**Status:** shipped.

## Locked

Stripe’s split, in our envelope:

| Field | Owner | Use |
|-------|--------|-----|
| `type` | machine | `invalid_request_error` \| `api_error` |
| `code` | machine | Closed set in `api-errors.md`. Clients branch on this + HTTP status. |
| `message` | human | One sentence. Safe to show in a UI. No regex, no `body/key must match`, no `VALIDATION_ERROR`. |
| `request_id` | support | Correlation. Not shown in product UI. |
| `details` | machine | Extra context. Optional. Never the only place the human sentence lives. |

Clients show `error.message`. They do not parse `details.fields` to invent copy.

Envelope shape does not change. Codes do not multiply into Stripe’s long `parameter_*` list. `details.fields` stays for multi-field 422s; `message` is still a complete sentence (first / most specific field).

## Stop

- Do not add Stripe `param` (we have `details.fields`).
- Do not add a catalog of Stripe-like codes (`parameter_missing`, …).
- Do not map plat5 fields in Happ (or any consumer) to invent sentences.
- Do not put `request_id`, HTTP status, or `code` in product UI.
- Do not leak internals in `message` (SQL, stack, bind-error junk). Bind failures stay a fallback sentence.
- Existence policy stays 404 + generic `Resource not found.` Do not say “you are not a member.”

## Fallback `message` per `code`

Use these only when a more specific sentence does not apply.

| Code | HTTP | Fallback `message` |
|------|------|-------------------|
| `INVALID_REQUEST` | 400 | Malformed request. |
| `VALIDATION_ERROR` | 422 | That doesn't look right. |
| `UNAUTHORIZED` | 401 | Authentication required. |
| `FORBIDDEN` | 403 | You don't have permission to do that. |
| `NOT_FOUND` | 404 | Resource not found. |
| `CONFLICT` | 409 | That already exists. |
| `PAYLOAD_TOO_LARGE` | 413 | Request body is too large. |
| `INTERNAL_ERROR` | 500 | An unexpected error occurred. |
| `SERVICE_UNAVAILABLE` | 503 | Service temporarily unavailable. |

`UNAUTHORIZED` / `NOT_FOUND` / `INTERNAL_ERROR` / `SERVICE_UNAVAILABLE` stay generic on purpose.

## Identity — specific `message`

`FieldError(path, message)` sets `message` as the product sentence. `details.fields[0]` repeats that sentence (path stays machine).

### Orgs

| When | `message` |
|------|-----------|
| Name empty | Name is required. |
| Name > 128 | Name is too long. |
| Slug not `[a-z0-9-]+` | Slug can only use lowercase letters, numbers, and dashes. |
| Slug taken | An organization with this slug already exists. |

### Members

| When | `message` |
|------|-----------|
| `user_id` empty | Choose someone to add. |
| `user_id` > 128 | That user ID is too long. |
| Bad role | Role must be member, admin, or owner. |
| Bad status | Status must be active, suspended, or removed. |
| Duplicate user in org | This person is already a member. |
| SA → owner | Service accounts cannot be owners. |
| Demote last owner | Cannot demote the sole owner. |
| Leave as last owner | Transfer ownership before leaving. |
| Suspend/remove last owner via status | Transfer ownership before changing the last owner's status. |
| Remove last owner | Transfer ownership before removing the last owner. |

### Service accounts

| When | `message` |
|------|-----------|
| Name empty | Name is required. |
| Name > 128 | Name is too long. |
| PATCH with no name | Nothing to update. |

### API keys (user + member)

| When | `message` |
|------|-----------|
| Name > 128 | Name is too long. |

Internal validate/resolve (`key` / `key_id` / `user_id`+`organization_id` required) are not product UI. Fallback 422 is enough. Do not polish those sentences for the dashboard.

### Pagination (`limit` / `offset`)

Rare in Happ UI. Fallback 422 is enough.

## Route-registry

`AppError::validation` puts the validation text in `message`. Operator-facing, not Happ.

Gateway-owned 401/404/413/500/503 keep the fallback table.

## Out of this repo

Happ web already shows `error.message` only. After identity ships, org/member/key alerts pick this up. Happ-owned 422/409 copy is already done in `e10s`.

Siblings (`cli`, `template-*`, `web-demo`): update only if they snapshot exact plat5 `message` strings.
