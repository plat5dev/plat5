# CLI

The `plat5` CLI, for the deployment's config. What a plan and an apply mean: [config](../concepts/config.md#plan-and-apply).

| Command | Does |
|---------|------|
| `plat5 validate [dir]` | Schema checks, offline |
| `plat5 plan [dir]` | Dry run; prints the diff |
| `plat5 plan [dir] -out plan.json` | Also saves the plan: the config, `expect` and `confirm` together |
| `plat5 apply [dir]` | Dry run, show the diff, ask to confirm, apply |
| `plat5 apply plan.json` | Apply a saved plan as it is. No prompt: saving it was the review |
| `plat5 apply [dir] --auto-approve` | Apply without the prompt. Refused when the plan is destructive, unless `--confirm folder=14` |
| `plat5 routes generate [dir]` | Write `catalog.yml` from the local `kinds.yml` ([generated catalog](api/edge-catalog.md)) |
| `plat5 config pull [--revision n] [dir]` | Write an applied revision out as a config directory |

The diff is Terraform's: `+` add, `~` change, `-` remove, each destructive line marked, then `Plan: 2 to add, 1 to change, 1 to remove.` Blocked changes print with what's in the way.

`plat5 plan --detailed-exitcode` exits `0` with no changes, `2` with changes, `1` on an error. A scheduled CI job running it against the repo's config shows drift: a deployment that no longer matches the repo.

**Rolling back** is applying an older config: `plat5 config pull --revision 38`, then `plat5 apply`. It goes through the same plan, so a rollback that removes a kind is destructive like any other.
