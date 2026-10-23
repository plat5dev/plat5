# CLI

The `plat5` CLI: a local stack on your machine, and the deployment's config. What a plan and an apply mean: [config](../concepts/config.md#plan-and-apply).

## The config

Every command takes the path to `plat5.yml`, default `./plat5.yml`, and follows its `services` list ([the config](../concepts/config.md#the-config)). It reaches the control plane's admin API at `--control-plane-url` (or `PLAT5_CONTROL_PLANE_URL`) with `--admin-token` (or `PLAT5_ADMIN_TOKEN`); unset, the local stack's, `http://localhost:5402` and `dev-admin-token`.

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

## The local stack

`plat5 init` and `plat5 start` run a project on your machine in Docker. They are laptop-only: a deployment is run as in [deploy](../guides/deploy.md), with `plat5 apply`.

| Command | Does |
|---------|------|
| `plat5 init [dir] --template none` | Write a project: a `plat5.yml` with `organization` and `project` kinds and roles for the generated labels, `routes/identity.yml` and `routes/audit.yml` as `routes generate` writes them, and `plat5.dev.yml` |
| `plat5 start` | Run the stack, apply `plat5.yml`, seed the dev person, print links and keys. Running it again updates the stack in place |

`plat5.dev.yml` holds how `start` runs the project on your machine: image versions, modules, your app's compose file, ports. A deployment never reads it, and it holds nothing that's in `plat5.yml`. Its shape is the CLI's: [CLI README](https://github.com/plat5dev/cli#plat5devyml).

**Apply.** Once the control plane is up, `start` plans `plat5.yml` as `plat5 apply` does. A plan that destroys nothing is applied without a prompt. A destructive plan is refused: `start` prints it and says to run `plat5 apply`, which asks to confirm. A service's `url` is the address the gateway reaches, as in any deployment; `start` puts your app's containers on the stack's network so their compose service names resolve, and with your app run on your machine instead, maps those names to it.

**Dev seed.** `start` then makes sure there's something to call with, through the [generated routes](api/edge-catalog.md) like any client: a dev token from local Auth carrying a `name` claim, so the dev person has a name; a node labelled `Dev` of each root kind other than the sign-in kind, found through `/me/nodes` and reused; and one personal key (`POST /me/keys`), which reaches every tree its person can. A config without those routes gets a warning, and `start` goes on.
