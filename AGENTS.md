# Plat5: agent contract

`docs/` is law. If code and a contract disagree, fix the one that's wrong, usually the code. Don't invent a third story in chat.

How I decide: `my-principles` skill. This file is how to work here, not what Plat5 is; it restates no rules, so it can't drift from them.

## What this is

A thin platform runtime for multi-tenant APIs: a gateway, identity (the tree of nodes, principals, members, bindings, keys), audit, and a control plane for the deployment's kinds, roles, routes and CORS origins.

## Where the rules live

| What | Where |
|------|-------|
| The model and its principles | [`docs/concepts/model.md`](docs/concepts/model.md) |
| The rules, one concept per doc | [`docs/concepts/`](docs/concepts/): access, lifecycle, config, audit |
| What Plat5 won't do (stop conditions) | [`docs/concepts/boundaries.md`](docs/concepts/boundaries.md#not-in-plat5) |
| Shapes: config documents, APIs, headers, env | [`docs/reference/`](docs/reference/) |
| How Plat5 stores things | [`docs/internals/`](docs/internals/) |
| Why each decision was made, by topic | [`docs/decisions/`](docs/decisions/README.md) |
| The map of all of it | [`docs/README.md`](docs/README.md) |
| What's next, and deferred work and when it's ready | [`TODO.md`](TODO.md) |

## Working here

1. Read `docs/concepts/model.md`, then the concept and reference docs for the area you're touching, before editing.
2. Don't implement past the contract. If the work needs a contract change, stop and propose the doc change first.
3. A change that touches a principle in `concepts/model.md` or an item under "Not in Plat5" in `concepts/boundaries.md` needs explicit sign-off. Say which one, and why, before writing code.
4. A rule belongs in exactly one concept doc, a shape in exactly one reference doc. Guides link to both and restate neither. Don't restate anything here.
5. Don't commit unless asked.

### Siblings

Wire or boot changes aren't done in this repo alone. They also update the `cli` (its embedded compose and control-plane client track HEAD), the `template-*` repos (the API starters and the two consoles), `toolbox`, and the `operator` repo. Run HEAD with `plat5 init --source plat5=../plat5`.
