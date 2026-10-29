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
| `plat5 routes list [plat5.yml]` | Every service's expanded routes, identity's and audit's included: method, public path, scope, upstream and permissions, sorted by path. It sends the config as a dry run and prints the routes the control plane expanded ([dry run](api/control-plane.md#dry-run)), so it needs a control plane, local or deployed |
| `plat5 config pull [--revision n] [dir]` | Write an applied revision out as `plat5.yml` plus `services/<service>.yml`, into a directory with no `.yml` files. Revisions are stored as sent, so it gives back what was applied |

The diff: `+` add, `~` change, `-` remove, each line with its changed fields, `before → after`, leaving out fields that are unset or empty. A service line lists its expanded routes the same way. Blocked lines print with what's in the way. Then the summary: `Plan: 2 to add, 1 to change, 0 to remove.`

After an apply, the CLI waits up to 10 seconds for every gateway replica to report the new revision ([reader status](api/control-plane.md#reader-status)), then prints `gateway: 3/3 replicas on revision 42`, or each replica that refused it and why. A refusal makes the command exit `1`, though the revision is applied.

`plat5 plan --detailed-exitcode` exits `0` with no changes, `2` with changes, `1` on an error. A scheduled CI job running it against the repo's config shows drift: a deployment that no longer matches the repo.

**Rolling back** is applying an older config: `plat5 config pull --revision 38`, then `plat5 apply`. It goes through the same plan.

## The local stack

`plat5 init` and `plat5 start` run a project on your machine in Docker. They are laptop-only: a deployment is run as in [deploy](../guides/deploy.md), with `plat5 apply`.

| Command | Does |
|---------|------|
| `plat5 init [dir]` | Ask for a starter and the modules. `-y` takes `--template none` and every module. `--no-auth`, `--no-observability` and `--no-operator` turn a module off |
| `plat5 init [dir] --template none` | Write a project: a `plat5.yml` with `organization`, `project` and `environment` kinds and the example roles, `services/identity.yml` (service accounts off at projects, invites off at environments) and `services/audit.yml`, `operator.routes.yml` from operator's shipped route list, and `plat5.dev.yml` |
| `plat5 init [dir] --template <starter>` | The same, plus an API from a starter, a customer console and an operator console ([starters](#starters)) |
| `plat5 start` | Run the stack, apply `plat5.yml`, seed the dev person, print links and keys. Running it again updates the stack in place |
| `plat5 stop` | Stop the stack and your app. Data is kept |
| `plat5 reset` | Stop the stack and your app, and delete the project's local data: its databases, your app's volumes, the dev person. Asks first; `-y` doesn't. The next `start` begins empty, as after `init`. For a stack whose schema changed in place |

`plat5.dev.yml` holds how `start` runs the project on your machine: image versions, modules, your app's compose file, the operator module's route file and console, ports. A deployment never reads it, and it holds nothing that's in `plat5.yml`. Its shape is the CLI's: [CLI README](https://github.com/plat5dev/cli#plat5devyml).

**Modules.** `auth`, `observability` and `operator`, each on unless `plat5.dev.yml` turns it off; `plat5 start -x <module>` skips one for a run. The `operator` module runs operator's gateway on the project's `operator.routes.yml`, operator-audit, an operator IdP (a second Plat5 Auth in dev mode, never the customer IdP) and the operator console, in the same compose project and Postgres. What each module runs, its ports and logins: [CLI README](https://github.com/plat5dev/cli#what-plat5-start-runs).

**Apply.** Once the control plane is up, `start` applies `plat5.yml` through the same plan as `plat5 apply`, without a prompt. The first apply, on a fresh deployment, prints only its summary rather than the whole config as a diff. A blocked plan is printed and refused. A service's `url` is the address the gateway reaches, as in any deployment; `start` puts your app's containers on the stack's network so their compose service names resolve, and with your app run on your machine instead, maps those names to it.

**Dev seed.** `start` then makes sure there's something to call with, through identity's routes like any client: a dev token from local Auth; the dev person's name, `Dev Person`, set with `PATCH /user`, since Plat5 Auth's tokens carry no name; a node named `Dev` of each root kind, found through `/user/nodes` and reused; and one personal key (`POST /user/keys`), which reaches every tree its person can. A config without those routes gets a warning, and `start` goes on.

### Starters

The starters are `go-fiber-api`, `node-fastify-api` and `bun-effect-api`, the repos `plat5dev/template-<name>`: one API, tasks inside an environment and the person's profile, in Go with Fiber, Node with Fastify, and Bun with Effect. `--template` also takes `owner/repo` on GitHub or a `.tar.gz` URL. A starter adds these to `--template none`'s files:

| File | What |
|------|------|
| `services/<service>.yml` | The starter's service file, listed in `plat5.yml`'s `services` |
| `api/` | The starter |
| `customer-console/` | The customer console, `plat5dev/template-customer-console`, which works with every starter. `--no-customer-console` leaves it out, and its origin with it |
| `plat5.yml` `cors.allowed_origins` | The console's origin, `http://localhost:3001`, so the browser can call the gateway ([CORS](config/cors.md)). `plat5.dev.yml`'s `app.web` is the same origin |
| `operator-console/` | The operator console, `plat5dev/template-operator-console`. `start` runs it with the operator module. `--no-operator-console` leaves it out |
| `docker-compose.yml` | Includes each app's own compose file. `plat5.dev.yml`'s `app.compose` points at it, so `start` runs the apps |
| `operator.routes.yml` | Operator's shipped route list, with the starter's operator routes added under its `services`. `start`'s operator gateway mounts it as its `ROUTES_FILE`, as an operator gateway you deploy does |
| `README.md` | What's in the project, and how to extend it |

A starter refuses a directory that has any of these files, before writing anything. Where templates are fetched from, and their manifest: [CLI README](https://github.com/plat5dev/cli#starters).
