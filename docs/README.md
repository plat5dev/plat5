# Plat5 docs

`docs/` is law. Concepts hold the rules, reference holds the shapes, guides walk through tasks and restate nothing. When two disagree, the concept doc wins, and [`concepts/model.md`](concepts/model.md) wins over the rest.

## Start here, by what you're doing

| You're | Read |
|--------|------|
| Learning Plat5 | [model](concepts/model.md), then [access](concepts/access.md) |
| Running a deployment | [deploy](guides/deploy.md), [change the config](guides/change-the-config.md) |
| Writing a service behind it | [write a service](guides/write-a-service.md) |
| Building a console or client | [build a console](guides/build-a-console.md) |
| Working on Plat5 itself | the concepts, then [internals](#internals) |

## Concepts

| Doc | What it decides |
|-----|-----------------|
| [model](concepts/model.md) | Kinds, nodes, principals, members, bindings; ids and slugs; the principles |
| [access](concepts/access.md) | The ancestor rule, who can be in a tree, roles and bindings, credentials, how a request is decided, lists |
| [lifecycle](concepts/lifecycle.md) | Creating, deleting, restoring and purging; members; lifecycle events |
| [config](concepts/config.md) | `plat5.yml` and routes files, validation, plan and apply, revisions |
| [audit](concepts/audit.md) | What's recorded, in which log, and the delivery guarantee |
| [boundaries](concepts/boundaries.md) | What Plat5 is for, what it won't do, extension points |

## Guides

| Doc | |
|-----|--|
| [deploy](guides/deploy.md) | Images, compose, TLS, attaching an app |
| [change the config](guides/change-the-config.md) | Add or remove kinds, change roles, roll back, CI drift checks |
| [write a service](guides/write-a-service.md) | What a service receives, stores, and must not do |
| [build a console](guides/build-a-console.md) | Signing in, navigating, access, invites, restore |

## Reference

| Doc | |
|-----|--|
| [config/kinds](reference/config/kinds.md), [roles](reference/config/roles.md), [routes](reference/config/routes.md), [cors](reference/config/cors.md) | Config documents. Field tables generated from `schema/v2/` |
| [api/edge-catalog](reference/api/edge-catalog.md) | Generated edge routes and their default labels |
| [api/identity](reference/api/identity.md) | Identity's service API |
| [api/control-plane](reference/api/control-plane.md) | The admin API |
| [api/audit](reference/api/audit.md) | Audit events and reads |
| [api/internal](reference/api/internal.md) | Admit, plan counts, feeds, audit writes |
| [cli](reference/cli.md) | The `plat5` CLI |
| [gateway](reference/gateway.md) | Gateway behavior: admit, cache, rate limits, CORS, client IP, boot |
| [headers](reference/headers.md) | Every `X-Plat5-*` header |
| [env](reference/env.md) | Every service's environment |
| [idp](reference/idp.md), [errors](reference/errors.md), [error copy](reference/error-copy.md), [lists](reference/lists.md), [naming](reference/naming.md), [telemetry](reference/telemetry.md), [health](reference/health.md), [container labels](reference/container-labels.md) | Conventions |

## Internals

| Doc | |
|-----|--|
| [identity schema](internals/identity-schema.md) | Tables and invariants |
| [audit storage](internals/audit-storage.md) | Partitions, roles, runtime |

## Decisions and transition

- [Decision records](decisions/README.md): why, with references.
- [v2 cutover](v2-cutover.md): transitional; deleted once v2 ships.
