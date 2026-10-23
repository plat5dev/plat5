# Plat5 API error response standard

Standard envelope across Plat5 services.

Clients show `error.message`. Branch on `code` and HTTP status. Do not parse `details` to invent copy. Product sentences live in [`error-copy.md`](error-copy.md).

## Envelope

```json
{
  "error": {
    "type": "invalid_request_error",
    "code": "VALIDATION_ERROR",
    "message": "Label is required.",
    "request_id": "abc-123",
    "details": {
      "fields": [
        { "path": "label", "message": "Label is required." }
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
| `FORBIDDEN` | 403 | `invalid_request_error` | Permission denied, or it may not exist. | `{ required_labels }` |
| `NOT_FOUND` | 404 | `invalid_request_error` | Resource not found. | `{ resource, id }` |
| `CONFLICT` | 409 | `invalid_request_error` | That already exists. | `{ field, value }` |
| `ROUTE_CONFLICT` | 409 | `invalid_request_error` | (control plane) Message lists each conflicting route and its owner service. | — |
| `PLAN_STALE` | 409 | `invalid_request_error` | The deployment changed since this plan was made. Plan again. | `{ revision_from, revision_now, identity_revision_from?, identity_revision_now? }` |
| `PAYLOAD_TOO_LARGE` | 413 | `invalid_request_error` | Request body is too large. | `{ max_size_bytes }` |
| `RATE_LIMITED` | 429 | `api_error` | Too many requests. Try again in a moment. | `{ retry_after_seconds }` |
| `INTERNAL_ERROR` | 500 | `api_error` | An unexpected error occurred. | `null` |
| `SERVICE_UNAVAILABLE` | 503 | `api_error` | Service temporarily unavailable. | `null` |

### `FORBIDDEN`

Returned by the **gateway** when the route's node doesn't exist, the caller isn't in its tree, or the caller's labels on the node share none with the route's `required_labels`. One body for all three, as Google Cloud answers "Permission denied on resource (or it may not exist)", so a 403 never says whether a node exists. `details.required_labels` is the route's list: any one of those labels would have admitted the request. The caller's own labels are never in the body; the gateway logs them on a `required_labels denied` line ([`telemetry.md`](telemetry.md#gateway-label-denied-line)).

### `NOT_FOUND`

From the gateway: no route matches. From a service's own API, called by id: the id isn't under the address given. Through the gateway, a missing node is a **403**, never a **404**.

### `UNAUTHORIZED`

Returned by the **gateway**, not downstream services: a bad, missing, revoked or suspended credential, or a service account key on a sign-in route. Identity unavailable is **503**. See [`gateway-contract.md`](gateway-contract.md#admit).

### `PLAN_STALE`

Returned by the **control plane** when a confirmed plan's config revision has moved, or when a plan with a destructive change finds identity's data revision moved. Nothing is applied ([`control-plane.md`](control-plane.md#the-plan)).

### `RATE_LIMITED`

Returned by the **gateway**. `Retry-After` (seconds) is the same value as `details.retry_after_seconds`. Two independent Valkey limiters; Valkey error or timeout → **503**. Admitted limited routes set `X-RateLimit-Limit` / `Remaining` / `Reset` on 2xx and 429; the failed-auth limiter sets `Retry-After` only.

| Limiter | When | Key |
|---------|------|-----|
| Admitted route | After admission, before the label check | A node at the policy's `count_at`, plus the policy or method + path ([`routes.md`](routes.md#buckets)) |
| Failed-auth IP | Unadmitted **401**s, not-found **403**s, unmatched **404**s. Not per-route | Client IP. `RATE_LIMIT_AUTH_FAILURE_REQUESTS` / `RATE_LIMIT_AUTH_FAILURE_WINDOW_SECONDS` (default 60/60). `0` = off |

## Principles

- **Consistent envelope.** Every error has the same top-level `error` object.
- **`message` is for humans.** One sentence. No regex, no `code` names, no bind-error junk.
- **Never leak internals.** `INTERNAL_ERROR` responses don't include stack traces. Log those server-side.
- **Never leak existence.** Through the gateway, missing and forbidden are the same **403**.
- **Gateway handles authn.** Gateway returns `UNAUTHORIZED`. Downstream services must not.
