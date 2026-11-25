# Error responses

Plat5's runtime services (gateway, identity, audit, control plane) return errors in one JSON envelope. Your services can follow this envelope to give clients a single error parser, a consistent `request_id` for support correlation, and human-safe `message` strings across every route — using whatever `code` values fit your domain.

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
| `type` | `string` | `invalid_request_error` (4xx) or `api_error` (429, 5xx) |
| `code` | `string` | Machine-readable identifier (`UPPER_SNAKE_CASE`). Clients branch on `code` + HTTP status |
| `message` | `string` | Human-readable sentence, safe to display in a UI. Never leaks SQL, stack traces, or internal details |
| `request_id` | `string` | Correlation ID from `X-Request-ID` |
| `details` | `object \| null` | Optional structured context. Never the only place the human message lives |

`request_id` is also returned in the `X-Request-ID` response header by the gateway on every public response.

## Standard codes (Plat5 services)

| Code | HTTP | Type | Details |
|------|------|------|---------|
| `INVALID_REQUEST` | 400 | `invalid_request_error` | — |
| `UNAUTHORIZED` | 401 | `invalid_request_error` | `{ reason }` |
| `FORBIDDEN` | 403 | `invalid_request_error` | `{ permissions }` on kind routes, or `null` |
| `NOT_FOUND` | 404 | `invalid_request_error` | `{ resource, id }` |
| `CONFLICT` | 409 | `invalid_request_error` | `{ field, value }` |
| `PAYLOAD_TOO_LARGE` | 413 | `invalid_request_error` | `{ max_size_bytes }` |
| `VALIDATION_ERROR` | 422 | `invalid_request_error` | `{ fields: [{ path, message }] }` |
| `RATE_LIMITED` | 429 | `api_error` | `{ retry_after_seconds }` |
| `INTERNAL_ERROR` | 500 | `api_error` | `null` |
| `SERVICE_UNAVAILABLE` | 502 / 503 | `api_error` | `null` |

Domain-specific `409` codes returned by individual Plat5 APIs (such as `PLAN_STALE` on the control plane, `CURSOR_AHEAD` on the lifecycle log, or `INVITE_NOT_REDEEMABLE` on identity) are documented with those endpoints. Your services are free to define their own `code` strings.

## Key gateway rules

- **Never leak existence (`FORBIDDEN`, 403):** On a kind route, a missing node, a caller outside the tree, and a missing permission all return the same **403** body (`"Permission denied, or it may not exist."`) with `details.permissions` (or `null` on `any_member` routes). The caller's own permissions are never in the body. A service account calling a user route gets **403** with `details: null`.
- **Gateway owns `UNAUTHORIZED` (401):** Returned for missing, malformed, expired, unknown, or revoked credentials, suspended people, or keys of service accounts on deleted nodes. Downstream services behind the gateway do not authenticate callers.
- **Status distinction in Plat5 APIs:** **400** for malformed requests before values are read; **422** for invalid input; **409** when current state refuses the request; **503** when a fail-closed dependency (Postgres, identity, external authorizer, or sync audit) is unavailable. (Exception: identity's AuthZEN `/access/v1/evaluation*` endpoints return **400** for invalid bodies per the AuthZEN spec.)
- **Rate limits (`RATE_LIMITED`, 429):** Carries `Retry-After` (seconds) matching `details.retry_after_seconds`, plus `X-RateLimit-*` on admitted route limits.
