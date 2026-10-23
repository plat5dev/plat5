# CLI

The `plat5` CLI, for the deployment's config. What a plan and an apply mean: [config](../concepts/config.md#plan-and-apply).

Every command takes the path to `plat5.yml`, default `./plat5.yml`, and follows its `services` list ([the config](../concepts/config.md#the-config)).

| Command | Does |
|---------|------|
| `plat5 validate [plat5.yml]` | Schema checks, offline. Errors name the file and the field |
| `plat5 plan [plat5.yml]` | Dry run; prints the diff |
| `plat5 plan [plat5.yml] -out plan.json` | Also saves the plan: exactly the apply body, the config plus `expect` and `confirm` |
| `plat5 apply [plat5.yml]` | Dry run, show the diff, ask to confirm, apply |
| `plat5 apply plan.json` | Apply a saved plan as it is. No prompt: saving it was the review |
| `plat5 apply [plat5.yml] --auto-approve` | Apply without the prompt. Refused when the plan is destructive, unless `--confirm folder=14` |
| `plat5 routes generate [plat5.yml]` | Write `routes/identity.yml` and `routes/audit.yml` from `plat5.yml`'s kinds, and add them to `services` if missing ([edge catalog](api/edge-catalog.md)). Overwrites those two files; git shows any hand edits it replaced |
| `plat5 config pull [--revision n] [dir]` | Write an applied revision out as `plat5.yml` plus `routes/<service>.yml`, into a directory with no `.yml` files. Revisions are stored as sent, so it gives back what was applied |

The diff is Terraform's: `+` add, `~` change, `-` remove, each destructive line marked, then `Plan: 2 to add, 1 to change, 1 to remove.` Blocked changes print with what's in the way.

`plat5 plan --detailed-exitcode` exits `0` with no changes, `2` with changes, `1` on an error. A scheduled CI job running it against the repo's config shows drift: a deployment that no longer matches the repo.

**Rolling back** is applying an older config: `plat5 config pull --revision 38`, then `plat5 apply`. It goes through the same plan, so a rollback that removes a kind is destructive like any other.
