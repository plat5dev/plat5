# CORS document

The origins and request headers the gateway allows, in `plat5.yml`. Request-time behavior: [`reference/gateway.md`](../gateway.md#cors).

```yaml
# in plat5.yml
cors:
  allowed_origins: ["https://app.example.com", "http://localhost:5173"]
  allowed_headers: [idempotency-key]
```

<!-- generated from schema: cors.json#/properties/cors/properties -->
| Field | Rule |
|-------|------|
| `allowed_origins` | Up to 100. `["*"]` allows any origin; `*` is valid only alone. Otherwise each is `https://host` or `http://host`, with an optional `:port`. Lowercase, no path, no trailing `/`, no repeats: the gateway compares it byte for byte with the browser's `Origin` |
| `allowed_headers` | Up to 32 header names, lowercase, added to the gateway's base list of allowed request headers |
<!-- end generated -->

An empty `allowed_origins`, or no `cors` in the config, allows no cross-origin requests: the gateway sends no `Access-Control-Allow-Origin`.
