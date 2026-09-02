# Gateway TODO

Deferred gateway work.

## Security Headers

Gateway does not add standard security headers. Desired on all API responses:

- `X-Content-Type-Options: nosniff`
- `X-Frame-Options: DENY`
- `Referrer-Policy: strict-origin-when-cross-origin`
- `Strict-Transport-Security` (when TLS is active)

## Rate Limiting

Open: `X-RateLimit-Limit` / `Remaining` / `Reset` headers; multi-instance aggregation.

## JWKS Conditional Refresh

JWKS is fetched on startup. Empty cache retries every 2s until loaded; loaded cache refreshes every 15 minutes. Startup fetch failure → degraded (`/health/ready` 503 until JWKS loads). Cold-path fetch holds the JWKS mutex across the network call.

Desired: respect `Cache-Control`, `ETag`, or `Last-Modified`; fetch outside the lock on the cold path.
