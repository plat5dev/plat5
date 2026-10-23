# Decisions

Why Plat5 is shaped the way it is, by topic, with the Kubernetes and Google Cloud references each choice follows. These are reasons, not contracts: the rules live in [`concepts/`](../concepts/) and [`reference/`](../reference/), and where a reason and a contract differ, the contract wins.

| Topic | Why… |
|-------|------|
| [Tree and kinds](tree-and-kinds.md) | The tree is config; nodes, ids, slugs and paths; the generated routes |
| [Access](access.md) | The ancestor rule; credentials and principals; assigning access; admit, rate limits and lists; what services see |
| [Config and apply](config-and-apply.md) | One config applied whole through a plan; revisions; readers; the CLI and the local stack; the checks that keep copies honest |
| [Lifecycle](lifecycle.md) | Soft delete, restore and purge; removing a kind; members; stored data |
| [Audit](audit.md) | The node chain on events; logs per root; reads by node |
| [Boundaries](boundaries.md) | Closed worlds; what Plat5 is for; sharing beyond one org; extension points; costs accepted |

When a decision is settled, its rule goes into the contract first, and its reason into the topic here, in the same change.
