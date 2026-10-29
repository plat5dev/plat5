# Plat5 API error response standard

Standard envelope across Plat5 services.

Clients show `error.message`. Branch on `code` and HTTP status. Do not parse `details` to invent copy. Product sentences live in [`reference/error-copy.md`](error-copy.md).

## Envelope

```json
{
  "error": {
    "type": "invalid_request_error",
    "code": "VALIDATION_ERROR",
    "message": "Display name is required.",
    "request_id": "abc-123",
    "details": {
      "fields": [
        { "path": "display_name", "message": "Display name is required." }
      ]
    }
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `type` | `string` | `invalid_request_error`, `api_error` |
| `code` | `string` | Machine-readable identifier (UPPER_SNAKE_CASE) |
| `message` | `string` | Human-readable sentence. Safe to show in a UI |
| `request_id` | `string` | Correlation ID from `X-Request-ID`. Propagate it; do not generate it |
| `details` | `object \| null` | Type-specific context. Shape varies by `code`. Never the only place the human sentence lives |

`request_id` is also returned in the **`X-Request-ID`** response header on every public API response. The gateway handles this; services behind the gateway must not set it.

**Not required on:** CORS preflight (`OPTIONS`) and internal health checks (`/health/live`, `/health/ready`).

## Error codes

The **Fallback message** column is used only when nothing more specific applies.

| Code | HTTP | Type | Fallback message | Details |
|------|------|------|------------------|---------|
| `INVALID_REQUEST` | 400 | `invalid_request_error` | Malformed request. | — |
| `VALIDATION_ERROR` | 422 | `invalid_request_error` | That doesn't look right. | `{ fields: [{ path, message }] }` |
| `UNAUTHORIZED` | 401 | `invalid_request_error` | Authentication required. | `{ reason }` |
| `FORBIDDEN` | 403 | `invalid_request_error` | Permission denied, or it may not exist. | `{ required_permissions }`, or `null` on an `any_member` or user route |
| `NOT_FOUND` | 404 | `invalid_request_error` | Resource not found. | `{ resource, id }` |
| `CONFLICT` | 409 | `invalid_request_error` | That already exists. | `{ field, value }` |
| `PLAN_STALE` | 409 | `invalid_request_error` | The deployment changed since this plan was made. Plan again. | `{ precondition, expected, actual }` |
| `PAYLOAD_TOO_LARGE` | 413 | `invalid_request_error` | Request body is too large. | `{ max_size_bytes }` |
| `RATE_LIMITED` | 429 | `api_error` | Too many requests. Try again in a moment. | `{ retry_after_seconds }` |
| `INTERNAL_ERROR` | 500 | `api_error` | An unexpected error occurred. | `null` |
| `SERVICE_UNAVAILABLE` | 503 | `api_error` | Service temporarily unavailable. | `null` |

### `FORBIDDEN`

Returned by the **gateway** when the route's node doesn't exist, the caller isn't in its tree, or the caller's permissions on the node share none with the route's `required_permissions`. One body for all three, so a 403 never says whether a node exists. `details.required_permissions` is the route's list: any one of them would have admitted the request. On an `any_member` route, `details` is `null`: there a 403 only means the caller isn't in the tree. The caller's own permissions are never in the body; the gateway logs them on a `required_permissions denied` line ([`reference/telemetry.md`](telemetry.md#gateway-permission-denied-line)).

Also returned by the **gateway** for a service account on a user route, with `details: null` and its own sentence ([error copy](error-copy.md#gateway-403)).

### `NOT_FOUND`

From the gateway: no route matches. From a service's own API, called by id: the id isn't under the address given. Through the gateway, a missing node is a **403**, never a **404**.

### `UNAUTHORIZED`

Returned by the **gateway**, not downstream services: a missing, malformed, unknown or revoked credential, a suspended person's, or the key of a service account whose node is deleted. A **401** always means the credential itself is bad; a known caller who may not make the request is a **403**. Identity unavailable is **503**. See [admit](api/internal.md#what-the-gateway-answers).

### `PLAN_STALE`

Returned by the **control plane** when an apply's preconditions fail: `precondition` is `revision`: someone applied since the dry run. Nothing is applied; dry-run again ([`concepts/config.md`](../concepts/config.md#preconditions)).

### `RATE_LIMITED`

Returned by the **gateway**. `Retry-After` (seconds) is the same value as `details.retry_after_seconds`. Two independent Valkey limiters; Valkey error or timeout → **503**. Admitted limited routes set `X-RateLimit-Limit` / `Remaining` / `Reset` on 2xx and 429; the failed-auth limiter sets `Retry-After` only.

| Limiter | When | Key |
|---------|------|-----|
| Admitted route | After admission, before the permission check | The policy and its bucket ([rate limits](config/rate-limits.md#buckets)) |
| Failed-auth IP | What identity refused: **401** for an invalid key or `valid: false`, and not-found **403**s. Not a service account's **403** on a user route. Not per-route | Client IP. `RATE_LIMIT_AUTH_FAILURE_REQUESTS` / `RATE_LIMIT_AUTH_FAILURE_WINDOW_SECONDS` (default 60/60). `0` = off |

## Principles

- **Consistent envelope.** Every error has the same top-level `error` object.
- **`message` is for humans.** One sentence. No regex, no `code` names, no bind-error junk.
- **Never leak internals.** `INTERNAL_ERROR` responses don't include stack traces. Log those server-side.
- **Never leak existence.** Through the gateway, missing and forbidden are the same **403**.
- **Gateway handles authn.** Gateway returns `UNAUTHORIZED`. Downstream services must not.
