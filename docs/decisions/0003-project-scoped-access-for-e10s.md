# 0003: Project-scoped access, as e10s needs it

- **Status:** Accepted
- **Date:** Oct 2026

The seven decisions e10s v2 needed from Plat5: project-scoped keys, the project in the path, what a project is, rate limits, a per-project activity log, authorship, and what may enter a tree's data. The seventh is its own record ([0004](0004-closed-world-and-member-ids.md)).

## How a check flows in v2

1. The gateway matches the route (`scope: project`) and reads the credential.
2. **New:** it resolves the slug chain (`{organization}`, `{project}`) to node ids.
3. Identity's admit answers for that credential on that chain: the member id, the labels from the union of bindings on the project and its ancestors, and a `good_until`. The answer is cached like credentials are.
4. The gateway checks the route's labels, then each rate-limit policy.
5. The request goes upstream with `X-Plat5-Organization-Id`, `X-Plat5-Project-Id`, `X-Plat5-Member-Id` and `X-Plat5-Principal`.

The one new step resolves the project slug to its node and checks that the key's binding sits on that project or above it. Every fact it needs is in Plat5, so there's no outside call; the result is cached like credentials are.

## Decision list

Seven decisions follow from the model. Six land in Plat5 v2 as part of the model itself; only authorship stays inside e10s. Change the dropdowns as you decide.

| # | Decision | Plat5 change | Recommendation | Status |
| --- | --- | --- | --- | --- |
| 1 | How a key is limited to one project | Needed | A service account bound to the project; the gateway checks bindings on the project and its ancestors. Members see no projects by default | Decided |
| 2 | How the path names the project | Needed | `/orgs/{organization}/projects/{project}/…` by slugs; the gateway resolves them and forwards ids | Decided |
| 3 | What a project is | Needed | A Plat5 node; e10s keys its data on the node id | Decided |
| 4 | Rate limits per org or per project | Needed | Each limit names the kind it counts at; monthly limits stay e10s counts | Decided |
| 5 | Per-project activity log | Needed | Audit events carry the node; filter by project | Decided |
| 6 | Who published or approved a version | None | Records store `X-Plat5-Member-Id` as their owner | Decided |
| 7 | What may enter a tree's data | Needed | Each tree is a closed world; people cross, data doesn't; bindings can expire | Decided |

## 1. Project-scoped keys

Locked: a key is exactly as wide as its service account's bindings, and a binding can sit on a project. A sandbox's machine key belongs to a service account bound only to the sandbox project, so it reaches that project and nothing else. Org members see no projects until a role is bound at a project, or at the org for all of them.

- **People** stay on the org's roster. A role at the org reaches every project below it; a role at one project reaches only that project.
- **Service accounts** can be born in a project. Deleting the project revokes and deletes them with their keys.
- **The gateway's check** is the ancestor rule: the key's binding must sit on the route's project or above it. Plat5 stores every fact it needs, so there's no outside call and no new failure mode.
- **The policy-engine hook** stays deferred, for per-resource permissions only, such as sharing one document. Projects don't need it.

This replaces the earlier options (org-wide keys, the hook, a project stored on the key, e10s checking the caller, an org per project): bindings give a narrow key without making a credential narrower than its membership.

Decided: a project role can exceed the org role. Access is the union of bindings on the node and its ancestors.

## 2. The path names the org and the project

Locked: paths carry the chain of slugs from the root. A person's credential reaches every org they belong to and project slugs are unique only within an org, so the org has to be in the path. The gateway resolves each slug to its node id and forwards only ids. Node ids are accepted in any position too.

```yaml
routes:
  - path: /orgs/{organization}/projects/{project}/subscribers/{subscriber_id}/entitlements/check
    scope: project
    upstream: /projects/{subject.project_id}/subscribers/{path.subscriber_id}/entitlements/check
    methods:
      POST:
        required_labels: [check]
        audit: false
```

This is how Kubernetes nests a namespace in its paths (`/namespaces/{ns}/pods/{name}`) and how GitHub addresses `/{owner}/{repo}`. The subject carries every ancestor, so `{subject.organization_id}` is there for services that want it.

- **Machines** put the org and project slugs in a config file once; they never change.
- **People** get readable console URLs.
- **Services** store ids only; slug handling lives once, in Plat5.

## 3. A project is a Plat5 node

Recommendation: Plat5 creates, labels, deletes and restores projects; e10s keeps everything inside one, keyed by the node id. The node holds only id, kind, parent, slug, label and status, so a project's catalog, subscribers and settings stay e10s data.

- **Create:** the console calls Plat5's route. Plat5 emits `project.created` and gives the creator `creator_role` on it; e10s creates its project-settings row from the event.
- **Delete:** soft for the grace period. The gateway refuses the project at once. On `project.deleted` e10s stops serving it but keeps its data; on `project.restored` it resumes; on `project.purged` it deletes the data.
- **Org delete:** cascades to its projects, each with its own events.

Still to decide: does e10s keep any project data that must exist before the first event arrives (default plan settings, for instance), or can it always create rows lazily?

## 4. Rate limits and metering

Recommendation: a rate-limit policy names the kind it counts at, so the burst limit on eval routes can count per org or per project. Monthly evals and subscribers stay e10s counts: org-wide against the org plan, broken down per project.

| Limit | Where it's counted | Org-wide | Per project |
| --- | --- | --- | --- |
| Burst, 100/s on eval routes | Gateway, Valkey | `count_at: organization` | `count_at: project`, so a sandbox can't spend a live project's budget |
| Monthly evals | e10s | Sum across projects; Free's allotment applies to the sum | One counter row per project |
| Subscribers | e10s, at subscriber create | Count across projects against the org plan | Count per project |

Today the gateway's limiter subject is fixed by scope. In the v2 model a route's subject carries every ancestor node, so choosing the level is a policy setting, not a special case.

Decided: both. Eval routes carry a project policy and an org policy, and a request must pass both.

## 5. Per-project activity log

Recommendation: every audit event records the node it touched and that node's ancestors, so "this project" and "this org and everything under it" are both filters. Reading stays behind `organization:audit:read`, or a role at the project for a project's own history.

Audit stays reporting only: product features never read it, so the plan changelog can't be built from it (decision 6).

## 6. Who published or approved a version

Locked: versions, proposals and approvals store the caller's member id, from `X-Plat5-Member-Id`, as their owner. The member id is stable per person per org, so "published by" and "don't notify me about my own change" compare like with like.

- **Any project route can record the owner.** No special binding routes; a service that prefers the id in its path rewrites `{subject.member_id}`.
- **Read** on the same project routes, so the changelog and review queue list everyone's records.
- **Separation of duties** ("you can't approve your own change") is e10s comparing two member ids: the proposal's and the approval's.
- **Departed members:** their records stay; the member lookup returns a removed member's name and status, so the changelog shows "Dana (former member)".
- **Operators** can supply a member id by acting as one, which the operator plane specifies in its own repo; a change made without one records no owner.
