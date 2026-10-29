# Plat5

Open-source **platform runtime** for multi-tenant APIs. You declare the tree's kinds in config (organization, project, …); Plat5 owns the nodes, who can reach each one, and the check at the gateway. A role bound at an org reaches every project under it; a role bound at one project reaches only that project. Services get node ids and the caller's member id, never a principal id, and do no auth themselves.

Point the gateway at any issuer of signed JWT access tokens ([`docs/reference/idp.md`](docs/reference/idp.md)), including Plat5 Auth if you run it separately.

The model: [`docs/concepts/model.md`](docs/concepts/model.md).

## Get started

Start with the [CLI](https://github.com/plat5dev/cli): `plat5 init` writes a project (a `plat5.yml`, identity's and audit's service files, the example roles) and `plat5 start` runs it on your machine. Its templates ([`plat5dev/template-*`](https://github.com/orgs/plat5dev/repositories)) are API starters and consoles built on Plat5. Then read the [docs](docs/README.md).

## Layout

| Path | Purpose |
|------|---------|
| `services/gateway/` | Reverse proxy, auth, routing (Rust / Pingora) |
| `services/control-plane/` | The deployment's config: validation, plans, admin API, revisions, feeds (Rust) |
| `services/identity/` | Nodes, users, service accounts, members, bindings, keys, admit (Go) |
| `services/audit/` | Audit log store and read API, per root and by node (Go) |
| `compose/` | Self-contained Plat5 stack, waiting for its first `plat5 apply` |
| `docs/` | Contracts |

## Documentation

| Doc | Contents |
|-----|----------|
| [`docs/README.md`](docs/README.md) | **Start here.** The map, by what you're doing |
| [`docs/concepts/model.md`](docs/concepts/model.md) | Kinds, nodes, principals, members, bindings, the principles |
| [`docs/concepts/access.md`](docs/concepts/access.md) | Who can do what, and how a request is decided |
| [`docs/guides/deploy.md`](docs/guides/deploy.md) | Production: images, TLS, attach an app |
| [`docs/guides/write-a-service.md`](docs/guides/write-a-service.md) | What a service behind Plat5 receives and must not do |
| [`docs/reference/`](docs/reference/) | Config documents, APIs, headers, env, errors, telemetry |
| [`AGENTS.md`](AGENTS.md) | How to work in this repo (for agents) |

## Telemetry

Stdout logs + `/metrics` always. OTLP opt-in (traces and metrics both on when endpoint set). [`docs/reference/telemetry.md`](docs/reference/telemetry.md).

## License

Apache-2.0 — see [LICENSE](LICENSE).
