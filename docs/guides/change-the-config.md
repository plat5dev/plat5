# Change the config

Keep `plat5.yml` and the service files in a repo, and change it only through `plat5 plan` and `plat5 apply` ([config](../concepts/config.md), [CLI](../reference/cli.md)).

## Add a kind at the bottom

Add it to `kinds` in `plat5.yml` with its `parent` and `path`, then `plat5 routes generate` to write identity's and audit's routes for it ([CLI](../reference/cli.md#generating-identitys-and-audits-routes)). Review the diff and delete again any route you had dropped. Grant its permissions in `roles`, add any service routes for it, and apply; the plan lists the new routes. `plat5 routes list` shows the result.

## Remove a kind

Delete its nodes first, through the node routes: the plan is blocked while the kind has nodes, deleted ones included, and lists some of them. Then remove it from `kinds`, regenerate identity's and audit's files (or delete that kind's routes from them), and remove every other route scoped to it, in one apply ([blocked changes](../concepts/config.md#blocked-changes)). Role permissions only its routes required can stay or go.

## Change a kind's parent

Only while the kind has no nodes, deleted ones included. Moving nodes between parents is deferred ([`TODO.md`](../../TODO.md)).

## Change roles

Edit `roles` in `plat5.yml` and apply. Removing a role, or a kind from its `bindable_at`, makes bindings that use it grant nothing; they grant again if a role with that name comes back, so don't reuse a removed role's name for something else.

## Change identity's or audit's routes

Edit `services/identity.yml` or `services/audit.yml` like any service file: delete a route to drop it, change a `path` or a verb's permissions, then apply ([generated routes](../reference/config/generated-routes.md)). A later `plat5 routes generate` overwrites the edits, so review its diff.

## Roll back

`plat5 config pull --revision n`, then `plat5 apply`. It goes through the same plan ([revisions](../concepts/config.md#revisions)).

## Check for drift in CI

Run `plat5 plan --detailed-exitcode` on a schedule against the repo's config. Exit `2` means the deployment no longer matches the repo.

## Review a plan before applying

`plat5 plan -out plan.json` saves the config and the revision it was planned against. Review it, then `plat5 apply plan.json`. If anyone applies in between, it's refused and you plan again.
