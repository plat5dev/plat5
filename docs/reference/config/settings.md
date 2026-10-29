# Settings

The deployment-wide settings in `plat5.yml`, beside `kinds`, `roles`, `rate_limits`, `cors` and `services`. Concept: [config](../../concepts/config.md).

```yaml
# in plat5.yml
deletion_grace: 30d
reserved_slugs: [new, settings, api, admin]
users:
  name:
    claims: [name, preferred_username]
    sync: first_sign_in
defaults:
  timeouts: { connect: 10s, read: 30s }
  max_body_bytes: 10485760
```

<!-- generated from schema: settings.json#/properties/deletion_grace settings.json#/properties/reserved_slugs settings.json#/properties/users settings.json#/$defs/users/properties settings.json#/$defs/users_name/properties settings.json#/properties/defaults settings.json#/$defs/defaults/properties -->
| Field | Rule |
|-------|------|
| `deletion_grace` | How long a deleted node stays restorable before it's purged. One value for every kind. A duration: a whole number and `s`, `m`, `h` or `d` (`30d`, `12h`). Default `30d`. A change applies to new deletes only |
| `reserved_slugs` | Slugs no node may take, deployment-wide |
| `users` | How Plat5 reads a person from the IdP's token |
| `name` | Where a person's name comes from at sign-in, and when a sign-in sets it |
| `claims` | The token claims that may hold the person's name, read in order: the first non-empty one wins. 1 to 8, each a dotted path like `AUTH_SUBJECT_CLAIM` (`name`, `profile.display_name`). Default `[name, preferred_username]` |
| `sync` | When a sign-in sets the name. `first_sign_in`: only while the person's name is empty. `each_sign_in`: on every sign-in with a non-empty claim, and the person can't change it themselves. Default `first_sign_in` |
| `defaults` | What a service gets for a field it leaves out |
| `timeouts` | `{ connect, read }`, each a duration. Default `10s` and `30s` |
| `max_body_bytes` | The largest request body the gateway forwards, in bytes. Default `10485760` (10 MiB) |
<!-- end generated -->

What `users.name` does at sign-in, and who else can set a person's name: [IdP](../idp.md#a-persons-name).

A service's own `timeouts` and `max_body_bytes` override these ([services](services.md#service-fields)). The default rate limit is the `default` policy, which every config has ([rate limits](rate-limits.md#on-a-route)).

The issuer, the JWKS, the accepted audiences and the subject claim are trust anchors, kept in the gateway's environment ([`reference/env.md`](../env.md#gateway)).
