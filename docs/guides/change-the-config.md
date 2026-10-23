# Change the config

Keep the config directory in a repo, and change it only through `plat5 plan` and `plat5 apply` ([config](../concepts/config.md), [CLI](../reference/cli.md)).

## Add a kind at the bottom

Add it to `kinds.yml` with its `parent`, regenerate the catalog (`plat5 routes generate`), add any service routes for it, and apply. Safe: one apply.

## Add a kind in the middle

A `folder` between org and project is two safe applies: add `folder` under `organization`, then widen `project`'s parent to `[organization, folder]`. Routes for projects in folders are new routes with `{folder}` in their chain.

## Remove a kind

Remove it from `kinds.yml`, and remove every route scoped to it or with its param in the chain, in the same apply. The plan marks it destructive with the node count; `plat5 apply` asks you to confirm that count. Its nodes are soft-deleted. Applying the kind again within the grace period restores them ([lifecycle](../concepts/lifecycle.md#removing-a-kind)).

## Change roles

Edit `roles.yml` and apply. Always safe: removing a role, or a kind from its `bindable_at`, makes bindings that use it grant nothing until it's back.

## Roll back

`plat5 config pull --revision n`, then `plat5 apply`. It goes through the same plan.

## Check for drift in CI

Run `plat5 plan --detailed-exitcode` on a schedule against the repo's config. Exit `2` means the deployment no longer matches the repo.

## Review a plan before applying

`plat5 plan -out plan.json` saves the config, the revision and the confirmed counts. Review it, then `plat5 apply plan.json`. If anyone applies in between, or a destructive count changes, it's refused and you plan again.
