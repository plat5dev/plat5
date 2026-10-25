# Settings

The deployment-wide settings in `plat5.yml`, beside `kinds`, `roles`, `rate_limits`, `cors` and `services`. Concept: [config](../../concepts/config.md).

```yaml
# in plat5.yml
deletion_grace: 30d
reserved_slugs: [new, settings, api, admin]
users:
  name_claim: name
defaults:
  timeouts: { connect: 10s, read: 30s }
  max_body_bytes: 10485760
```

<!-- generated from schema: settings.json#/properties/deletion_grace settings.json#/properties/reserved_slugs settings.json#/properties/users settings.json#/$defs/users/properties settings.json#/properties/defaults settings.json#/$defs/defaults/properties -->
| Field | Rule |
|-------|------|
| `deletion_grace` | How long a deleted node stays restorable before it's purged. One value for every kind. A duration: a whole number and `s`, `m`, `h` or `d` (`30d`, `12h`). Default `30d`. A change applies to new deletes only |
| `reserved_slugs` | Slugs no node may take, deployment-wide |
| `users` | How identity reads a person from the IdP's token |
| `name_claim` | The IdP claim holding a person's name. Default `name` |
| `defaults` | What a service gets for a field it leaves out |
| `timeouts` | `{ connect, read }`, each a duration. Default `10s` and `30s` |
| `max_body_bytes` | The largest request body the gateway forwards, in bytes. Default `10485760` (10 MiB) |
<!-- end generated -->

A service's own `timeouts` and `max_body_bytes` override these ([services](services.md#service-fields)). The default rate limit is the `default` policy ([rate limits](rate-limits.md)).

The issuer, the JWKS, the accepted audiences and the subject claim are trust anchors, kept in the gateway's environment ([`reference/env.md`](../env.md#gateway)).
