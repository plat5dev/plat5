# Plat5 docs

Concepts hold the rules, reference holds the wire and config shapes, guides walk through tasks, and internals document Plat5's own implementation. When two docs disagree, the concept doc wins, and [`concepts/model.md`](concepts/model.md) wins over the rest.

## Start here

| You're | Read |
|--------|------|
| Learning Plat5 | [model](concepts/model.md), then [access](concepts/access.md) |
| Writing a service behind it | [write a service](guides/write-a-service.md) |
| Building a console or client | [build a console](guides/build-a-console.md) |
| Self-hosting a deployment | [deploy](guides/deploy.md) |
| Working on Plat5 itself | the concepts, then [internals](#runtime-standards--internals) |

## Core contract

### Concepts

| Doc | What it decides |
|-----|-----------------|
| [model](concepts/model.md) | Kinds, nodes, principals, users, members, bindings; ids and slugs; the principles |
| [access](concepts/access.md) | The ancestor rule, who can be in a tree, roles and bindings, credentials, request decision order, outside-request checks, lists, UI route checks |
| [lifecycle](concepts/lifecycle.md) | Creating, deleting, restoring and purging; members; the lifecycle log |
| [config](concepts/config.md) | Control-plane config, validation, plan and apply, revisions, readers |
| [audit](concepts/audit.md) | What's recorded, in which log, delivery guarantee, optional service enrichment headers |
| [boundaries](concepts/boundaries.md) | What Plat5 is for, what it won't do, extension points |

### Reference

| Doc | Contents |
|-----|----------|
| [config/settings](reference/config/settings.md), [kinds](reference/config/kinds.md), [roles](reference/config/roles.md), [rate limits](reference/config/rate-limits.md), [cors](reference/config/cors.md), [services](reference/config/services.md) | Control-plane config shapes (field tables generated from `schema/`) |
| [config/generated-routes](reference/config/generated-routes.md) | Reference routes for exposing `identity` and `audit` through the gateway |
| [api/identity](reference/api/identity.md) | Identity service API |
| [api/control-plane](reference/api/control-plane.md) | Control plane admin API |
| [api/audit](reference/api/audit.md) | Audit events and read API |
| [api/internal](reference/api/internal.md) | Admit, plan counts, lifecycle log, AuthZEN access evaluation, control-plane feeds, audit writes |
| [gateway](reference/gateway.md) | Gateway behavior: admit, cache, rate limits, `/access/routes`, CORS, client IP, boot |
| [headers](reference/headers.md) | `X-Plat5-*` headers |
| [env](reference/env.md) | Environment variables for every runtime service |
| [idp](reference/idp.md) | JWT access token contract and person names |

## Guides

| Doc | Contents |
|-----|----------|
| [write a service](guides/write-a-service.md) | What a service must do for isolation, and which Plat5 standards to follow for extra capabilities |
| [build a console](guides/build-a-console.md) | Signing in, navigating, `/access/routes`, invites, restore |
| [deploy](guides/deploy.md) | Self-hosting images, Compose, TLS, optional modules, upgrades |

## Runtime standards & internals

How Plat5's own four services behave — and optional standards user services can follow for uniform client errors, pagination, and end-to-end traces.

| Doc | Contents |
|-----|----------|
| [errors](reference/errors.md) | Error response envelope and Plat5's standard codes |
| [lists](reference/lists.md) | Cursor pagination used by `identity` and `audit` |
| [telemetry](reference/telemetry.md) | Logs, Prometheus `/metrics`, OTLP traces/metrics, and `traceparent` |
| [health](reference/health.md) | `/health/live` and `/health/ready` on `INTERNAL_PORT` |
| [identity schema](internals/identity-schema.md) | Postgres tables and DB invariants for `identity` |
| [audit storage](internals/audit-storage.md) | Partitions and storage internals for `audit` |
| [module map](internals/module-map.md) | Internal module dependency map across the 4 services |
