# Gateway TODO

This file tracks deferred gateway improvements that are intentionally out of scope for the current pass.

## CORS Configuration

- **Done:** `ALLOWED_ORIGINS` (comma-separated). Empty (default) → `Access-Control-Allow-Origin: *`. Non-empty → allowlist; matching `Origin` is reflected with `Vary: Origin`.

## Security Headers

- **Current state:** Gateway does not add standard security headers.
- **Desired state:** Add the following on all API responses:
  - `X-Content-Type-Options: nosniff`
  - `X-Frame-Options: DENY`
  - `Referrer-Policy: strict-origin-when-cross-origin`
  - `Strict-Transport-Security` (when TLS is active)
- **Rationale:** Baseline security hygiene for any API serving browser clients.

## Rate Limiting

- **Done (this slice):** In-process token bucket. Admitted routes inherit gateway fallback (`RATE_LIMIT_REQUESTS` / `WINDOW`); subject from route scope (`public`→ip, `user`→user, `organization`→org). Per-route override or `false` opt-out. Separate always-on failed-auth IP limiter (`RATE_LIMIT_AUTH_FAILURE_*`) for unadmitted 401s and unmatched 404s. Envelope `RATE_LIMITED` + `Retry-After`. No Redis.
- **Still open:** `X-RateLimit-Limit` / `Remaining` / `Reset` headers; multi-instance aggregation.

## JWKS Conditional Refresh

- **Current state:** JWKS is fetched eagerly on startup (`initialize`) and refreshed unconditionally every 15 minutes in the background. If startup fetch fails, the gateway starts degraded (`/health/ready` → 503 until JWKS loads); lazy fetch on first JWT request still holds the JWKS mutex across the network call.
- **Desired state:** Respect `Cache-Control`, `ETag`, or `Last-Modified` headers from the issuer to skip unnecessary fetches. Optionally avoid holding the mutex across the network on the cold path (fetch outside lock, then swap).
- **Rationale:** Reduces issuer load; cleaner concurrency under JWKS outage recovery.

## JWT Audience Validation

- **Done:** Added `AUTH_ALLOWED_AUDIENCES` (comma-separated client IDs). When non-empty, `validate_aud` is enabled and the list is enforced. Empty (default during stubbing) preserves previous "off" behavior.
