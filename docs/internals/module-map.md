# Module map

Internal dependency structure of the four Plat5 services (`services/gateway` and `services/control-plane` in Rust, `services/identity` and `services/audit` in Go), `e2e`, and `schema/`. Layering follows: wiring → handlers → domain → infrastructure leaves.

## 1. Services (runtime)

```mermaid
flowchart LR
  client([clients]) --> gateway
  gateway -->|admit, evaluations<br/>IDENTITY_INTERNAL_URL| identity
  gateway -->|"/internal/gateway feed<br/>CONTROL_PLANE_URL"| cp[control-plane]
  gateway -->|audited writes<br/>AUDIT_URL| audit
  gateway --> valkey[(Valkey<br/>rate limits)]
  identity -->|identity feed: kinds, roles<br/>blocks boot| cp
  cp -.->|plan counts, only when a plan<br/>removes a kind or changes a parent| identity
  identity --> pg[(Postgres)]
  audit --> pg
  cp --> pg
  schema[/schema/*.json/] -->|build.rs, compiled in| cp
  e2e[e2e tests] -.->|HTTP, whole stack| gateway
```

The dotted `control-plane → identity` edge closes a cycle with `identity → control-plane`. It is deliberate and documented: `docs/concepts/config.md#blocked-changes`.

## 2. gateway (Rust crate)

```mermaid
flowchart TD
  main --> lib --> gw[gateway<br/>request handler]
  gw -->|4| pipeline
  gw -->|3| route_map
  gw -->|3| rate_limit
  gw -->|3| error
  gw -->|2| audit
  gw --> control_plane
  gw --> config
  gw --> auth
  gw --> cors
  gw --> client_ip
  gw --> telemetry
  gw --> internal_http
  pipeline -->|4| route_map
  pipeline -->|2| auth
  pipeline -->|2| rate_limit
  pipeline --> error
  pipeline --> kinds
  route_map --> kinds
  route_map --> duration
  error --> route_map
  auth -->|2| error
  auth -->|2| internal_http
  audit --> auth
  audit --> error
  audit --> internal_http
  control_plane --> route_map
  control_plane --> cors
  control_plane --> auth
  control_plane --> kinds
  config --> auth
  config --> client_ip
  config --> duration
  health --> auth
  health --> rate_limit
  telemetry --> metrics
```

No cycles. `metrics` is used by `auth` (4 files), `gateway`, `pipeline`, `audit`, `control_plane` and `telemetry`; those edges are left out of the drawing. `route_map` now holds `upstream` (a route's upstream template, compiled when the route is built), and `pipeline` fills it per request.

## 3. control-plane (Rust crate)

```mermaid
flowchart TD
  main --> api
  main --> store
  main --> feed
  main --> counts
  api --> apply
  api --> served
  api --> store
  api --> schema
  api --> error
  api --> telemetry
  apply --> plan
  apply --> store
  apply --> served
  apply --> rules
  apply --> schema
  apply --> counts
  apply --> config
  plan --> served
  plan --> expand
  plan --> schema
  plan --> counts
  plan --> config
  store --> served
  served --> expand
  served --> config
  rules --> expand
  rules --> schema
  rules --> config
  expand --> config
  feed --> store
  error --> schema
  telemetry --> metrics
```

No cycles. Test-only modules (`api_tests`, `docgen`, `fixtures`) are left out. Every edge has weight 1: each module is one file. `served` is the projection of a checked config into the two feed bodies. `apply` orchestrates (check, plan, write), `plan` compares two `Served`, and `store` persists them.

## 4. identity (Go module), packages grouped

`infra` = `config`, `db`, `errors`, `metrics`, `middleware`, `telemetry`. `internal` = `internal/httpx`, `internal/dbx`, `internal/id`, `internal/auditx`, `internal/secret` (`internal/testdb` is used only by tests).

```mermaid
flowchart TD
  main[identity main] --> members
  main --> nodes
  main --> lifecycle
  main --> feed
  main --> infra
  members -->|9| nodes
  members -->|4| lifecycle
  members -->|10| feed
  members -->|"9 dbx, 9 id, 3 httpx, 3 secret, 1 auditx"| internal
  members -->|9 errors| infra
  nodes --> lifecycle
  nodes -->|2| feed
  nodes -->|"2 dbx, 2 httpx, 1 id, 1 auditx"| internal
  nodes -->|3 errors| infra
  lifecycle --> internal
  lifecycle --> infra
  internal --> infra
```

## 5. audit (Go module)

```mermaid
flowchart TD
  main[audit main] --> events
  main --> config & db & middleware & telemetry
  events -->|2| errors
  events -->|2| metrics
  events -->|2| id[internal/id]
  events --> dbx[internal/dbx]
  events --> httpx[internal/httpx]
  dbx --> metrics
  httpx --> errors
  middleware --> errors & metrics & telemetry
```

No cycles. `events` is the only domain package.

## Modules

| Module | Responsibility | Depends on | Used by |
|---|---|---|---|
| **gateway** (service) | The public edge: routing, auth, rate limits, CORS, audit emission | identity, control-plane, audit, Valkey | clients, e2e |
| gw `gateway` | Pingora request handler: wires every module into the proxy | nearly every module | `lib` |
| gw `pipeline` | Per-request decision: admit, permissions, rate limits, upstream fill | route_map, auth, rate_limit, error, kinds, metrics | gateway |
| gw `route_map` | The route table built from the control-plane snapshot, each route's upstream template included | kinds, duration | gateway, pipeline, control_plane, error |
| gw `auth` | JWT validation, admit calls to identity | error, internal_http, metrics | gateway, pipeline, audit, config, control_plane, health |
| gw `control_plane` | Follows the control-plane feed, swaps in snapshots | route_map, cors, auth, kinds, metrics | gateway |
| gw `cors` | The CORS policy compiled from the control plane's `cors` document | none | gateway, control_plane |
| gw `client_ip` | Trusted proxies and the client IP and scheme they imply | none | gateway, config |
| gw `duration` | The duration format shared by env and config documents | none | config, route_map |
| gw `audit` | Sends audited writes to audit | auth, error, internal_http, metrics | gateway |
| gw `rate_limit`, `kinds`, `config`, `error`, `health`, `internal_http`, `metrics`, `telemetry` | Leaves and infrastructure | mostly none (`config` → auth, client_ip, duration; `error` → route_map) | many |
| **control-plane** (service) | Stores and validates the deployment's config, serves feeds | identity (counts), Postgres, `schema/` | gateway, identity |
| cp `api` | HTTP API: plan, apply, feeds | apply, store, schema, error, telemetry, metrics | main |
| cp `apply` | Checks a config, plans it against the head, writes the revision | plan, store, served, rules, schema, counts, config | api |
| cp `served` | The two feed bodies a revision serves, built from a checked config | expand, config | api, apply, plan, store |
| cp `plan` | Compares the head's served bodies with the new ones; blocked changes | served, expand, schema, counts, config | apply |
| cp `rules`, `expand`, `schema`, `config` | Config validation and expansion | schema, config, expand | apply, plan |
| cp `store` | Postgres storage of revisions | served | api, apply, feed, main |
| cp `feed`, `counts` | Long-poll feeds; HTTP client for identity's plan counts | store / none | main, apply, plan |
| **identity** (service) | The tree of nodes, principals, members, bindings, keys | control-plane (feed), Postgres | gateway, control-plane |
| id `members` | Users, people, service accounts, invites, bindings, admit, evaluations, internal API | nodes, lifecycle, feed, internal/*, errors, metrics | main |
| id `nodes` | Node tree, slugs, sweep, plan counts | lifecycle, feed, internal/*, errors, metrics | main, members |
| id `lifecycle` | The lifecycle event log | errors, internal/dbx, internal/httpx | main, members, nodes |
| id `feed` | Follows the control plane's identity feed | none | main, members, nodes |
| id `internal/*` | HTTP and DB helpers, ids, audit details, secrets | errors, metrics | members, nodes, lifecycle |
| id infra | config, db, errors, metrics, middleware, telemetry | none (middleware → errors, metrics, telemetry) | everything |
| **audit** (service) | Append-only audit log | Postgres | gateway |
| au `events` | Write and list audit events | errors, metrics, internal/* | main |
| **e2e** | Whole-stack tests over HTTP | the running stack | nobody |
| **schema/** | The config's JSON Schema | none | control-plane (build time) |

## Findings, most serious first

1. **`identity/members` has the highest fan-out of any module (10 packages) and is the largest (12 files, about 4,100 lines).** It holds four of the five tree concepts `AGENTS.md` names (principals, members, bindings, keys), plus the decision the gateway asks for on every request (`admit.go`, `evaluate.go`) and the internal API. Its dependency on `nodes` goes through the public API (`nodes.Get`, `nodes.LockedWritable`, `nodes.ErrNotFound`), so this is about size, not a broken boundary. Splitting one package per concept would likely add coupling, since bindings, members and principals are tightly linked.
   *Direction:* if it keeps growing, split decision from records: admit and evaluations in one package, member, invite and service-account writes in another. That is the split the gateway already makes between `pipeline` and the modules holding data.

2. **Identity and audit duplicate their infrastructure packages.** `telemetry`, `middleware`, `errors`, `metrics`, `internal/dbx`, `internal/httpx`, and `internal/id` are near-copies between Go services, and `telemetry.rs` is duplicated between the two Rust crates. Services are kept independent (no shared runtime library); keep standard-conformance checks honest when touching shared behavior.

3. **The identity ⇄ control-plane cycle between services.** Identity won't listen until it has the control plane's first config. The control plane asks identity for plan counts. Only identity knows whether a kind is still in use, so this is real ownership, not a tangle. Boot can't deadlock, because counts are asked only when a plan removes a kind or changes a parent, and `docs/concepts/config.md` says so.
   *Direction:* none needed. Keep the "a plan that does neither never calls identity" rule covered by e2e, since that rule is what keeps boot order safe.

4. **Naming: the same job has two names.** Identity's follower of the control plane is the package `feed`; the gateway's is the module `control_plane`.
   *Direction:* unify the names when either is next touched.

5. **High fan-in modules are small, stable leaves.** In the gateway, `auth` has 6 users and `metrics` 7. Some reach into `auth`'s submodules (`auth::jwt::Audiences`, `auth::admit::NodeRef`), but those are declared `pub mod`, so they are `auth`'s public interface, not its internals. In identity, `errors` and `internal/dbx` are used by every domain package. This is what high fan-in should look like.

6. **No orphaned or dead modules.** `identity/internal/testdb` and the control plane's `docgen` and `fixtures` are test-only on purpose. `e2e` has no non-test code by design.
