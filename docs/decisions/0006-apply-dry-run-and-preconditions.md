# 0006: Apply: dry run and preconditions, whole config

- **Status:** Accepted. Supersedes the stored-plan design in [0001](0001-tree-of-kinds.md) and [0002](0002-soft-delete-and-planned-changes.md)
- **Date:** Oct 2026

The first draft stored plans on the server behind a `plan_id` and checked staleness with a data revision bumped by every node or binding write.

## Decision

No stored plans. A dry run returns the plan; the apply recomputes it and carries two preconditions, the config revision the dry run saw and the node count of each kind it destroys. This is Kubernetes' server-side dry run ([dryRun](https://kubernetes.io/docs/reference/using-api/api-concepts/#dry-run)) and `resourceVersion` precondition ([concurrency control](https://kubernetes.io/docs/reference/using-api/api-concepts/#resource-versions)), with Terraform's rule that a stale saved plan is refused ([saved plans](https://developer.hashicorp.com/terraform/cli/commands/plan#out-filename)).

- **Counts, not a data revision.** A counter bumped by every node or binding write would make a destructive plan stale forever on a busy deployment. Confirming "14 nodes" only goes stale when the thing being destroyed changes.
- **The whole config, every apply.** The cross-document rules already tie kinds, roles and routes together, so a partial apply had to be checked against everything else anyway. Terraform's model: what's not in the config is removed, and the plan says so. Cost: a service team can't push its routes alone. Kubernetes' ApplySets are the pattern if separate owners are ever needed.
- **One schema, `version: 2`.** A published JSON Schema replaces hand-aligned copies of validation, as a CRD's schema does. The version key lets the next format be a migration.
- **The CLI owns the saved plan and the diff**, with `--detailed-exitcode` for drift checks in CI ([Terraform](https://developer.hashicorp.com/terraform/cli/commands/plan#detailed-exitcode)). Rollback is applying an old revision's config through the same plan.
- **Deferred:** rollout status, like `kubectl rollout status` (`TODO.md`).
