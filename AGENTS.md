# Plat5: agent contract

`docs/` is law. If code and a contract disagree, fix the one that's wrong, usually the code. Don't invent a third story in chat.

How I decide: `my-principles` skill. This file is how to work here, not what Plat5 is; it restates no rules, so it can't drift from them.

## What this is

A thin platform runtime for multi-tenant APIs: a gateway, identity (the tree of nodes, principals, members, bindings, keys), audit, and a control plane for the deployment's kinds, roles, routes and CORS origins.

## Where the rules live

| What | Where |
|------|-------|
| The model, its principles and rules | [`docs/model.md`](docs/model.md) |
| What Plat5 won't do (stop conditions) | [`docs/model.md#not-in-plat5`](docs/model.md#not-in-plat5), and each doc's "Not here" or "Non-goals" |
| Why each decision was made | [`docs/decisions.md`](docs/decisions.md) |
| Every wire contract | [`docs/README.md`](docs/README.md) |
| Deferred work and when it's ready | [`TODO.md`](TODO.md) |
| v1 → v2 transition and build order | [`docs/v2-cutover.md`](docs/v2-cutover.md) (until v2 ships) |

## Working here

1. Read `docs/model.md`, then the docs for the area you're touching, before editing.
2. Don't implement past the contract. If the work needs a contract change, stop and propose the doc change first.
3. A change that touches a principle in `model.md` or an item under "Not in Plat5" needs explicit sign-off. Say which one, and why, before writing code.
4. A rule belongs in exactly one doc. Link to it; don't restate it elsewhere, including here.
5. Don't commit unless asked.
