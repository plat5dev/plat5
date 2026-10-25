# CLI

The `plat5` CLI: a local stack on your machine, and the deployment's config. What a plan and an apply mean: [config](../concepts/config.md#plan-and-apply).

## The config

Every command takes the path to `plat5.yml`, default `./plat5.yml`, and follows its `services` list ([the config](../concepts/config.md#the-config)). It reaches the control plane's admin API at `--control-plane-url` (or `PLAT5_CONTROL_PLANE_URL`) with `--admin-token` (or `PLAT5_ADMIN_TOKEN`); unset, the local stack's, `http://localhost:5402` and `dev-admin-token`.

| Command | Does |
|---------|------|
| `plat5 validate [plat5.yml]` | Schema checks, offline. Errors name the file and the field |
| `plat5 plan [plat5.yml]` | Dry run; prints the diff |
| `plat5 plan [plat5.yml] -out plan.json` | Also saves the plan: exactly the apply body, the config plus `expect` |
| `plat5 apply [plat5.yml]` | Dry run, show the diff, ask to confirm, apply, then report where the gateway's replicas stand |
| `plat5 apply plan.json` | Apply a saved plan as it is. No prompt: the saved plan shows every change |
| `plat5 apply [plat5.yml] --auto-approve` | Apply without the prompt |
| `plat5 routes list [plat5.yml]` | Every service's expanded routes, identity's and audit's included: method, public path, scope, upstream and permissions, sorted by path. Offline |
| `plat5 config pull [--revision n] [dir]` | Write an applied revision out as `plat5.yml` plus `services/<service>.yml`, into a directory with no `.yml` files. Revisions are stored as sent, so it gives back what was applied |

The diff is Terraform's: `+` add, `~` change, `-` remove, each line with its changed fields, `before → after`. A service line lists its expanded routes the same way. Blocked lines print with what's in the way. Then the summary: `Plan: 2 to add, 1 to change, 0 to remove.`

After an apply, the CLI waits up to 10 seconds for every gateway replica to report the new revision ([reader status](api/control-plane.md#reader-status)), then prints `gateway: 3/3 replicas on revision 42`, or each replica that refused it and why. A refusal makes the command exit `1`, though the revision is applied.

`plat5 plan --detailed-exitcode` exits `0` with no changes, `2` with changes, `1` on an error. A scheduled CI job running it against the repo's config shows drift: a deployment that no longer matches the repo.

**Rolling back** is applying an older config: `plat5 config pull --revision 38`, then `plat5 apply`. It goes through the same plan.

## The local stack

`plat5 init` and `plat5 start` run a project on your machine in Docker. They are laptop-only: a deployment is run as in [deploy](../guides/deploy.md), with `plat5 apply`.

| Command | Does |
|---------|------|
| `plat5 init [dir] --template none` | Write a project: a `plat5.yml` with `organization` and `project` kinds and the example roles, `services/identity.yml` and `services/audit.yml`, and `plat5.dev.yml` |
| `plat5 start` | Run the stack, apply `plat5.yml`, seed the dev person, print links and keys. Running it again updates the stack in place |

`plat5.dev.yml` holds how `start` runs the project on your machine: image versions, modules, your app's compose file, ports. A deployment never reads it, and it holds nothing that's in `plat5.yml`. Its shape is the CLI's: [CLI README](https://github.com/plat5dev/cli#plat5devyml).

**Apply.** Once the control plane is up, `start` applies `plat5.yml` through the same plan as `plat5 apply`, without a prompt. A blocked plan is printed and refused. A service's `url` is the address the gateway reaches, as in any deployment; `start` puts your app's containers on the stack's network so their compose service names resolve, and with your app run on your machine instead, maps those names to it.

**Dev seed.** `start` then makes sure there's something to call with, through identity's routes like any client: a dev token from local Auth carrying a `name` claim, so the dev person has a name; a node named `Dev` of each root kind, found through `/user/nodes` and reused; and one personal key (`POST /user/keys`), which reaches every tree its person can. A config without those routes gets a warning, and `start` goes on.
