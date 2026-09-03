# Plat5 API Error Response Standard

Standard envelope across Plat5 services.

Clients show `error.message`. Branch on `code` and HTTP status. Do not parse `details` to invent copy.

Product sentences live in [`error-copy.md`](error-copy.md).

## Envelope

```json
{
  "error": {
    "type": "invalid_request_error",
    "code": "VALIDATION_ERROR",
    "message": "Slug can only use lowercase letters, numbers, and dashes.",
    "request_id": "abc-123",
    "details": {
      "fields": [
        { "path": "slug", "message": "Slug can only use lowercase letters, numbers, and dashes." }
      ]
    }
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `type` | `string` | `invalid_request_error`, `api_error` |
| `code` | `string` | Machine-readable identifier (UPPER_SNAKE_CASE) |
| `message` | `string` | Human-readable sentence. Safe to show in a UI. |
| `request_id` | `string` | Correlation ID from `X-Request-ID`. Propagate it; do not generate it. |
| `details` | `object \| null` | Type-specific context. Shape varies by `code`. Never the only place the human sentence lives. |

`request_id` is also returned in the **`X-Request-ID`** response header on every public API response. The gateway handles this; services behind the gateway must not set it.

**Not required on:** CORS preflight (`OPTIONS`) and internal health checks (`/health/live`, `/health/ready`).

## Error Codes

The **Fallback message** column is used only when nothing more specific applies. `UNAUTHORIZED`, `NOT_FOUND`, `INTERNAL_ERROR`, and `SERVICE_UNAVAILABLE` stay generic on purpose.

| Code | HTTP | Type | Fallback message | Details |
|------|------|------|------------------|---------|
| `INVALID_REQUEST` | 400 | `invalid_request_error` | Malformed request. | — |
| `VALIDATION_ERROR` | 422 | `invalid_request_error` | That doesn't look right. | `{ fields: [{ path, message }] }` |
| `UNAUTHORIZED` | 401 | `invalid_request_error` | Authentication required. | `{ reason }` |
| `FORBIDDEN` | 403 | `invalid_request_error` | You don't have permission to do that. | `{ permission, resource, resource_id }` |
| `NOT_FOUND` | 404 | `invalid_request_error` | Resource not found. | `{ resource, id }` |
| `CONFLICT` | 409 | `invalid_request_error` | That already exists. | `{ field, value }` |
| `PAYLOAD_TOO_LARGE` | 413 | `invalid_request_error` | Request body is too large. | `{ max_size_bytes }` |
| `RATE_LIMITED` | 429 | `api_error` | Too many requests. Try again in a moment. | `{ retry_after_seconds }` |
| `INTERNAL_ERROR` | 500 | `api_error` | An unexpected error occurred. | `null` |
| `SERVICE_UNAVAILABLE` | 503 | `api_error` | Service temporarily unavailable. | `null` |

### `UNAUTHORIZED`

Returned by the **gateway**, not downstream services. If a downstream service receives a request without expected gateway-injected headers for its scope, that is a gateway bug → `INTERNAL_ERROR` (500). See [`gateway-contract.md`](gateway-contract.md) and [`identity-boundary.md`](identity-boundary.md).

### Organization admission (gateway)

Canonical policy: [`identity-boundary.md`](identity-boundary.md).

When `organization` scope is live: non-member / inactive / unknown org → **`NOT_FOUND` (404)**. Member resolve or key validate unavailable → **`SERVICE_UNAVAILABLE` (503)**. Bad credential → **`UNAUTHORIZED` (401)**.

### `RATE_LIMITED`

Returned by the **gateway**. HTTP **429**. `Retry-After` (seconds) is set to the same value as `details.retry_after_seconds`.

Two independent Valkey limiters (replicas share one budget). `VALKEY_URL` is required to boot. Valkey error → **503** `SERVICE_UNAVAILABLE`. Named policies and buckets: [`routes.md`](routes.md). Admitted limited routes set `X-RateLimit-Limit` / `Remaining` / `Reset` on 2xx and 429. Failed-auth limiter sets `Retry-After` only.

| Limiter | When | Key |
|---------|------|-----|
| Admitted route | After match + admission (JWT and API key). Omitted `rate_limit` inherits `RATE_LIMIT_REQUESTS` / `RATE_LIMIT_WINDOW_SECONDS` (never silent unlimited). `false` opts out. Inline object = this route+method. Policy name = service `rate_limits` entry (`shared: true` is opt-in cross-service). | Subject from route scope (`public`→ip, `user`→user, `organization`→org), plus method+path or policy name. |
| Failed-auth IP | Unadmitted **401**s and unmatched **404**s. Not per-route. | Client IP. `RATE_LIMIT_AUTH_FAILURE_REQUESTS` / `RATE_LIMIT_AUTH_FAILURE_WINDOW_SECONDS` (default 60/60). `0` requests = off |

## Principles

- **Consistent envelope** — Every error has the same top-level `error` object.
- **`message` is for humans** — One sentence. No regex, no `code` names, no bind-error junk.
- **Never leak internals** — `INTERNAL_ERROR` responses do not include stack traces. Log those server-side.
- **Gateway handles authn** — Gateway returns `UNAUTHORIZED`. Downstream services must not.
