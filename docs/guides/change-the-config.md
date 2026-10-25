# Change the config

Keep `plat5.yml` and the service files in a repo, and change it only through `plat5 plan` and `plat5 apply` ([config](../concepts/config.md), [CLI](../reference/cli.md)).

## Add a kind at the bottom

Add it to `kinds` in `plat5.yml` with its `parent`, `path` and `list_permission`, grant its permissions in `roles`, add any service routes for it, and apply. Identity and audit give it their routes ([resources](../reference/config/resources.md)); the plan lists them. `plat5 routes list` shows the result.

## Remove a kind

Delete its nodes first, through the node routes: the plan is blocked while the kind has live nodes, and lists some of them. Then remove it from `kinds`, with every route scoped to it and every role permission only its routes required, in one apply ([blocked changes](../concepts/config.md#blocked-changes)).

## Change a kind's parent

Only while the kind has no nodes. Moving nodes between parents is deferred ([`TODO.md`](../../TODO.md)).

## Change roles

Edit `roles` in `plat5.yml` and apply. Removing a role, or a kind from its `bindable_at`, makes bindings that use it grant nothing; they grant again if a role with that name comes back, so don't reuse a removed role's name for something else.

## Change identity's or audit's routes

Override their resources in `services/identity.yml` or `services/audit.yml`: a path, one verb's permissions, or `false` to remove a resource or a verb ([resources](../reference/config/resources.md)).

## Roll back

`plat5 config pull --revision n`, then `plat5 apply`. It goes through the same plan.

## Check for drift in CI

Run `plat5 plan --detailed-exitcode` on a schedule against the repo's config. Exit `2` means the deployment no longer matches the repo.

## Review a plan before applying

`plat5 plan -out plan.json` saves the config and the revision it was planned against. Review it, then `plat5 apply plan.json`. If anyone applies in between, it's refused and you plan again.
