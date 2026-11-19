# Settings

Deployment-wide settings in the control-plane config, beside `kinds`, `roles`, `rate_limits`, `cors` and `services`. All fields here are optional. Concept: [config](../../concepts/config.md).

```yaml
deletion_grace: 30d
reserved_slugs: [new, settings, api, admin]
defaults:
  rate_limit: [standard]
  timeouts: { connect: 10s, read: 30s }
  max_body_bytes: 10485760
```

<!-- generated from schema: settings.json#/properties/deletion_grace settings.json#/properties/reserved_slugs settings.json#/properties/defaults settings.json#/$defs/defaults/properties -->
| Field | Rule |
|-------|------|
| `deletion_grace` | How long a deleted node stays restorable before it's purged. One value for every kind. A duration: a whole number and `s`, `m`, `h` or `d` (`30d`, `12h`). Default `30d`. A change applies to new deletes only |
| `reserved_slugs` | Slugs no node may take, deployment-wide |
| `defaults` | What a route or a service gets for a field it leaves out |
| `rate_limit` | One or more policies from `rate_limits`, unique, none with `count_at`: what a route without `rate_limit` uses, and what the gateway counts `/access/routes` against ([rate limits](rate-limits.md#on-a-route)). Omitted: routes without `rate_limit` are unlimited |
| `timeouts` | `{ connect, read }`, each a duration. Default `10s` and `30s` |
| `max_body_bytes` | The largest request body the gateway forwards, in bytes. Default `10485760` (10 MiB) |
<!-- end generated -->

A route's own `rate_limit`, and a service's own `timeouts` and `max_body_bytes`, override these ([services](services.md#service-fields)).

IdP trust anchors and claim mappings (`AUTH_ISSUER`, `AUTH_JWKS_URI`, `AUTH_ALLOWED_AUDIENCES`, `AUTH_SUBJECT_CLAIM`, `AUTH_NAME_CLAIMS`) and module switches (`AUDIT_ENABLED`, `RATE_LIMIT_ENABLED`) are kept in the gateway's environment ([`reference/env.md`](../env.md#gateway)).
