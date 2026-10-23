# CORS document

The origins the gateway allows. Request-time behavior: [`reference/gateway.md`](../gateway.md#cors).

```yaml
version: 2
cors:
  allowed_origins: ["https://app.example.com", "http://localhost:5173"]
```

| Field | Rule |
|-------|------|
| `allowed_origins` | Up to 100. Each is `https://host` or `http://host`, with an optional `:port`. Lowercase, no path, no trailing `/`, no wildcard, no repeats: the gateway compares it byte for byte with the browser's `Origin` |

An empty list, or no `cors` in the config, answers CORS with `*`. Otherwise the gateway echoes an origin in the list and sends no `Access-Control-Allow-Origin` for any other. Request-time behavior: [`reference/gateway.md`](../gateway.md#cors).
