# Decisions

Why Plat5 is shaped the way it is, by topic. These are reasons, not contracts: the rules live in [`concepts/`](../concepts/) and [`reference/`](../reference/), and where a reason and a contract differ, the contract wins.

| Topic | Why… |
|-------|------|
| [Tree and kinds](tree-and-kinds.md) | The tree is config; nodes, ids, slugs and paths; identity's and audit's resources |
| [Access](access.md) | The ancestor rule; credentials and principals; assigning access; admit; access outside a request; what a UI shows; the edge; rate limits and lists; what services see |
| [Config and apply](config-and-apply.md) | Service files; roles in the config; validating once; one config applied whole through a plan; revisions; readers; the CLI and the local stack; the checks that keep copies honest |
| [Lifecycle](lifecycle.md) | Soft delete, restore and purge; kinds; members; the two event feeds; stored data |
| [Audit](audit.md) | What an event records; when it's written; logs; reads by node |
| [Boundaries](boundaries.md) | Closed worlds; what Plat5 is for; sharing beyond one org; extension points; costs accepted |

When a decision is settled, its rule goes into the contract first, and its reason into the topic here, in the same change.
