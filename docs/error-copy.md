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
- Do not invent sentences from `details` or field paths. Show `error.message`.
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
| `RATE_LIMITED` | 429 | Too many requests. Try again in a moment. |
| `INTERNAL_ERROR` | 500 | An unexpected error occurred. |
| `SERVICE_UNAVAILABLE` | 503 | Service temporarily unavailable. |

`UNAUTHORIZED` / `NOT_FOUND` / `INTERNAL_ERROR` / `SERVICE_UNAVAILABLE` stay generic on purpose.

`RATE_LIMITED` stays this sentence. Do not interpolate remaining seconds into `message` (`details.retry_after_seconds` / `Retry-After` carry that).

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

### Invites

| When | `message` |
|------|-----------|
| `expires_in_seconds` out of range | Expiry must be between 60 seconds and 30 days. |
| `email` > 320 | That email is too long. |
| Bad role | Role must be member, admin, or owner. |

Unknown / expired / revoked / already-used tokens use the generic **404** `Resource not found.` (existence policy; do not name the org). Already a member on a still-valid token is **200**, not 409.

### API keys (user + member)

| When | `message` |
|------|-----------|
| Name > 128 | Name is too long. |
| Scope label not `[a-z0-9:._-]+` | That scope label isn't valid. |
| Scope label > 64 chars | That scope label is too long. |
| More than 32 scopes | Too many scopes. |
| Duplicate scope labels | Scope labels must be unique. |

Internal validate/resolve (`key` / `key_id` / `user_id`+`organization_id` required) are not product UI. Fallback 422 is enough.

### Pagination (`limit` / `offset`)

Fallback 422 is enough.

## Route-registry

`AppError::validation` puts the validation text in `message`. Operator-facing.

Gateway-owned 401/403/404/413/429/500/503 keep the fallback table.

Siblings (`cli`, `template-*`, `web-demo`): update only if they snapshot exact plat5 `message` strings.
