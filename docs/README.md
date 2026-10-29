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
| [model](concepts/model.md) | Kinds, nodes, principals, users, members, bindings; ids and slugs; the principles |
| [access](concepts/access.md) | The ancestor rule, who can be in a tree, roles and bindings, credentials, how a request is decided, access outside a request, lists, what a UI shows |
| [lifecycle](concepts/lifecycle.md) | Creating, deleting, restoring and purging; members; tree and user events |
| [config](concepts/config.md) | `plat5.yml` and service files, validation, plan and apply, revisions, readers |
| [audit](concepts/audit.md) | What's recorded, in which log, and the delivery guarantee |
| [boundaries](concepts/boundaries.md) | What Plat5 is for, what it won't do, extension points |

## Guides

| Doc | |
|-----|--|
| [deploy](guides/deploy.md) | Images, compose, TLS, attaching an app |
| [change the config](guides/change-the-config.md) | Add or remove kinds, change roles and identity's routes, roll back, CI drift checks |
| [write a service](guides/write-a-service.md) | What a service receives, stores, and must not do; keeping tenants apart; nodes with service fields; checking access outside a request |
| [build a console](guides/build-a-console.md) | Signing in, navigating, what to show, nodes with service fields, access, invites, restore |

## Reference

| Doc | |
|-----|--|
| [config/settings](reference/config/settings.md), [kinds](reference/config/kinds.md), [roles](reference/config/roles.md), [rate limits](reference/config/rate-limits.md), [cors](reference/config/cors.md), [services](reference/config/services.md) | Config documents. Field tables generated from `schema/` |
| [config/resources](reference/config/resources.md) | Identity's and audit's routes: their resources, defaults and default permissions |
| [api/identity](reference/api/identity.md) | Identity's service API |
| [api/control-plane](reference/api/control-plane.md) | The admin API |
| [api/audit](reference/api/audit.md) | Audit events and reads |
| [api/internal](reference/api/internal.md) | Admit, plan counts, access evaluation, feeds and reader reports, audit writes |
| [cli](reference/cli.md) | The `plat5` CLI |
| [gateway](reference/gateway.md) | Gateway behavior: admit, cache, rate limits, route checks, CORS, client IP, boot |
| [headers](reference/headers.md) | Every `X-Plat5-*` header |
| [env](reference/env.md) | Every service's environment |
| [idp](reference/idp.md), [errors](reference/errors.md), [error copy](reference/error-copy.md), [lists](reference/lists.md), [naming](reference/naming.md), [telemetry](reference/telemetry.md), [health](reference/health.md), [container labels](reference/container-labels.md) | Conventions |

## Internals

| Doc | |
|-----|--|
| [identity schema](internals/identity-schema.md) | Tables and invariants |
| [audit storage](internals/audit-storage.md) | Partitions, roles, runtime |

## Decisions

- [Decisions](decisions/README.md): why, by topic, with references.
- [`TODO.md`](../TODO.md): what's next, and deferred work.
