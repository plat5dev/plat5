# Plat5 API Error Response Standard

Standard envelope for machine-readable errors across Plat5 services.

## Envelope

```json
{
  "error": {
    "type": "invalid_request_error",
    "code": "VALIDATION_ERROR",
    "message": "Request validation failed",
    "request_id": "abc-123",
    "details": {
      "fields": [
        { "path": "email", "message": "Expected string" }
      ]
    }
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `type` | `string` | `invalid_request_error`, `api_error` |
| `code` | `string` | Machine-readable identifier (UPPER_SNAKE_CASE) |
| `message` | `string` | Human-readable description |
| `request_id` | `string` | Correlation ID from `X-Request-ID`. Propagate it; do not generate it. |
| `details` | `object \| null` | Type-specific context. Shape varies by `code`. |

`request_id` is also returned in the **`X-Request-ID`** response header on every public API response. The gateway handles this; services behind the gateway must not set it.

**Not required on:** CORS preflight (`OPTIONS`) and internal health checks (`/health/live`, `/health/ready`).

## Error Codes

| Code | HTTP | Type | Message | Details |
|------|------|------|---------|---------|
| `INVALID_REQUEST` | 400 | `invalid_request_error` | Malformed request | — |
| `VALIDATION_ERROR` | 422 | `invalid_request_error` | Request validation failed | `{ fields: [{ path, message }] }` |
| `UNAUTHORIZED` | 401 | `invalid_request_error` | Authentication required | `{ reason }` |
| `FORBIDDEN` | 403 | `invalid_request_error` | Insufficient permissions | `{ permission, resource, resource_id }` |
| `NOT_FOUND` | 404 | `invalid_request_error` | Resource not found | `{ resource, id }` |
| `CONFLICT` | 409 | `invalid_request_error` | Resource already exists | `{ field, value }` |
| `PAYLOAD_TOO_LARGE` | 413 | `invalid_request_error` | Request body exceeds maximum allowed size | `{ max_size_bytes }` |
| `INTERNAL_ERROR` | 500 | `api_error` | An unexpected error occurred | `null` |
| `SERVICE_UNAVAILABLE` | 503 | `api_error` | Service temporarily unavailable | `null` |

### `UNAUTHORIZED`

Returned by the **gateway**, not downstream services. If a downstream service receives a request without expected gateway-injected headers for its scope, that is a gateway bug → `INTERNAL_ERROR` (500). See [`gateway-contract.md`](gateway-contract.md) and [`identity-boundary.md`](identity-boundary.md).

### Organization admission (gateway)

Canonical policy: [`identity-boundary.md`](identity-boundary.md).

When `organization` scope is live: non-member / inactive / unknown org → **`NOT_FOUND` (404)**. Member resolve or key validate unavailable → **`SERVICE_UNAVAILABLE` (503)**. Bad credential → **`UNAUTHORIZED` (401)**.

## Principles

- **Consistent envelope** — Every error has the same top-level `error` object.
- **Never leak internals** — `INTERNAL_ERROR` responses do not include stack traces. Log those server-side.
- **Gateway handles authn** — Gateway returns `UNAUTHORIZED`. Downstream services must not.
