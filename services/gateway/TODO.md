# Gateway TODO

Deferred gateway work.

## Valkey drop hangs

`VALKEY_URL` is required to boot. A Valkey **command error** maps to **503**. A **dropped** Valkey (container stop, dead TCP) does not: `allow()` / ready PING wait on `ConnectionManager` with no response or connect timeout. Requests hang. After Valkey returns, the manager can stay wedged until the gateway process restarts.

Ready looks like: connect + command timeout (hundreds of ms) → **503** `SERVICE_UNAVAILABLE`; traffic recovers when Valkey is back without a gateway restart. No in-process limiter fallback.

## JWKS HTTP cache validators

JWKS refresh is interval-based (empty: 2s; loaded: 15 minutes). Fetch runs outside the JWKS lock.

Desired: respect `Cache-Control`, `ETag`, or `Last-Modified`.
