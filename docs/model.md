# Plat5 model

> Plat5's model: what each concept is, where each decision lives, and why. The other docs are how it shows up on the wire; when they disagree with this page, they are wrong.

Plat5 is the front door for multi-tenant APIs. On every request it answers three questions: who is calling, which realm the request belongs to, and whether this route allows it. It then forwards the request with the owner in the path. The service never learns who called.

## Concepts

| Concept | What it is |
|---|---|
| **Identity** | A person or a service account. Holds what is true in every realm: IdP links, a person's global status, existence and deletion. |
| **Realm** | A context that holds memberships. Identities are born in one, credentials belong to a membership in one, and each realm owns its audit log. There are two kinds: the **directory** and **organizations**. |
| **Directory** | The one realm every person belongs to. Persons are born here, with an internal id linked to their IdP subjects. Its roster is private. It has no roles, admins, invites or sessions; the IdP's JWT does the session's job. |
| **Organization** | The tenant: the isolation boundary. Service accounts are born here. It holds the roster, roles, invites and sessions. Everything inside belongs to exactly one. |
| **Membership** | An identity's standing in a realm. In an organization it carries a role and a status. A person's directory membership is their anchor there: it holds their own resources and keys, and has no status or role of its own. |
| **Credential** | A user JWT, an API key or a session. Always belongs to a membership. Carries exactly that membership's permissions. |
| **Role** | The deployment's name for a set of labels, from its roles document in the control plane. Plat5 names no roles. |
| **Label** | A permission a route can require. Only the gateway checks labels. |
| **Scope** | Who owns what a route addresses: `public` (no one), `organization` (the org), or the caller's own membership: `user` in the directory, `member` in an organization. |
| **Subject** | The owner's ids that the gateway fills into `upstream`. |
| **Audit** | The record of who did what. |

**Member** and **user** are the two everyday names for a membership: a member is a membership in an organization, and a user is a person in the directory.

## Principles

1. **A scope names who owns the resource.** It never names who is calling.
2. **Services see owners, never actors.** A service gets the subject in its path. It never learns which person or key called. There is no actor header.
3. **Attribution belongs to audit.** Services store owners, never authors. Plat5 has no creator concept ([below](#who-did-it)).
4. **Two front doors, one service contract.** The customer gateway and the staff gateway call the same service paths. A service can't depend on anything only one of them supplies.
5. **The credential names the realm; the client doesn't.** The organization comes from the credential, never from a path the client chose.
6. **Plat5 decides only over facts it stores: realms, memberships and roles.** The gateway decides which routes a membership may call, from its role's labels.
7. **An optional policy engine decides access to individual resources.** The gateway asks it about one resource ("may this member touch this project?"), through a hook. A service may ask it to list what its subject can reach, on the member's own scope (below). Neither plat5 nor services decide it.
8. **Services enforce only what holds no matter who asks.** That means state rules ("archived is read-only"), and scoping every lookup by the subject.
9. **A credential is never narrower than its membership.** To give a machine less, give it a service account with a smaller role.
10. **Everyone with access is on the organization's roster.** There are no outside members.
11. **The deployment's data, not plat5's names.** Roles come from the deployment's roles document: a file the operator writes and applies to the control plane, like routes. Plat5 ships defaults and gives none of them special status.
12. **An event goes in the log of the realm that owns what it changed.**

## Where each decision lives

| Question | Decided by |
|---|---|
| Who is this? | The IdP or identity, from the credential |
| Which realm? | The credential |
| May they call this route? | Role labels, at the gateway |
| May they touch this resource id? | The optional policy engine, through the gateway hook |
| Which of these resources can they reach? | The optional policy engine, asked by the service on a `member` route about its subject |
| Is this valid for the resource's current state? | The service |
| Who did it? | Audit |

## Where a field goes

When a service is tempted to store a reference to a member, ask what should happen to it if that member left tomorrow.

| Answer | It's… | It lives in |
|---|---|---|
| Nothing; it's a record of what happened | History | Audit |
| Someone else takes it over | A relationship, like assignee or reporter | Service data. Set by the client, governed by edit permission |
| It stays theirs and gives them rights | Member-owned, like a comment | `member` scope. Service data with a rule for departure |
| It goes away with them | Member-owned, like an API key or a subscription | Identity, or `member` scope. Cleaned up on `member.removed` |

Only principals are **owners**, and the scope names the owner. What a resource is removed along with is its **parent**, which is service data.

## What I can reach

The gateway can allow or deny a route, but it can't filter a list: it doesn't read bodies, and the service doesn't know who asked. So "the projects this member can see" is a route on the member's own scope: `GET /member/projects`. The service gets `member_id` as the subject and asks the engine which projects it can reach. Without an engine, the answer is every project in the org.

- **Precedent:** `/user/organizations` already lists the orgs a person can reach, on the person's own scope.
- **It isn't member scope used to learn who called.** The answer is a fact about the member, and nothing is stamped with their id.
- **It passes the front-door test.** Staff calling `/members/{member_id}/projects` get a meaningful answer: what that member can reach.

`GET /org/projects` stays "every project in the org", for whoever its labels admit.

## Who did it

Plat5 has no actor header and no creator concept. Every "created by" is one of three things:

- **Owner:** logic depends on the maker, so the maker owns it.
- **Relationship:** a settable link, like Jira's reporter. The client sets it; permission to edit is the only question.
- **History:** a truthful fact about the past. Read it from audit.

**When who-did-it matters to logic, the act is itself a record owned by the member who did it.** The org resource (task, doc, expense) has no creator. Members' contributions to it (comments, approvals, revisions, subscriptions) are member-owned, created on `member` scope. That is not using member scope to learn who called: the member owns the record that is created. A contribution has two route families: create (and edit your own) on `member` scope; read and moderate on `organization` scope.

**Product features never read audit.** Audit is for reporting and compliance. It can be off for a whole deployment, it is behind a label, and it may get retention. A feature that needs to know who did something needs an owner or a relationship.

Why:

- **It produces better designs.** Every case that looked like it needed the actor came out better without it:
  - A **subscription** instead of notifying `created_by`. A departed member stops getting notified, people can unsubscribe, and a task can have several watchers.
  - An **approval record** instead of an actor check, for separation of duties. The service compares two owners (the expense's and the approval's), a rule that holds whoever asks. Several approvers, quorum, revoking and history come for free.
  - **Member-owned revisions** instead of one creator per document. Authorship is per contribution, survives the member leaving, and doesn't depend on audit.
- **Front-door test.** Something created through the staff gateway would have a `created_by` that is null, or an id that means nothing in the org. A field only one front door can fill isn't data.
- **What an actor header would bring back:** access checks inside services, fields that are null through one door, personal data spread across services (audit keeps only a member id), and stale references to departed members.
- **It locks out no business need.** The cost is modelling work, not a missing capability. For a trivial display ("last changed by Bob") it's more ceremony than a `created_by` column. That trade is accepted.

## Examples

| Thing | Owner | Parent | Notes |
|---|---|---|---|
| Project | Org | Org | `organization` scope. Who created it is in audit. |
| Task | Org | Project | Collaborative: anyone who can reach the project can edit it. |
| Assignee, reporter | – | – | Relationships. Service data. |
| Comment, approval, revision | Its member | Task, expense, doc | Written on `member` scope. Stays, read-only, when the member leaves. |
| Subscription | Its member | Task | Removed along with the member. |
| API key | The membership | – | Removed along with it. A user key belongs to the directory membership, a member key to the org membership. |
| Service account | Org | Org | Reaches the whole org, within its role. Less than that needs the engine, or a separate org. |
| Contractor on one project | Org | – | An org member. Limiting them to one project is the engine's job. |
| Private project | Org | Org | Org members can reach it unless the policy engine says no. Lists of "my projects" come from `GET /member/projects`. |

## Audit

- Each realm owns its log.
- **Organization log:** events on `organization` and `member` routes, plus `user` routes that change something the org owns: creating it, joining it, minting a session for one of its members. The service names the org; the gateway never takes it from the path. Read with `org:audit:read`. Finer reads, like one project's history for non-admins, are a policy-engine question.
- **Personal log:** user keys, profile, status, account deletion. Read only by the person and by staff.
- **The id a create produced:** the service names it, and the event records it, so "who created X" can be looked up by X.
- No retention.

## Lifecycle

Identity writes lifecycle events in the same transaction as the change. Services opt in by reading a pull feed. The events are births and ends of organizations and members: `organization.created`, `organization.deleted` (one per delete, not cascaded per member), `member.created`, and `member.removed`. An org create writes `organization.created`, then `member.created` for the founder. `removed` is final: re-adding someone creates a new member id and a new `member.created`. Deleting an account emits `member.removed` in each organization it touches, and nothing for the directory. Updates, suspends, and the directory are not events.

People:

- **First sign-in creates the person.** Admission resolves the IdP issuer and subject to an internal id, creating the person if needed. Cached.
- **Adding a member takes the IdP subject**, which the deployer's backend knows. Identity resolves the person, creating them if needed. Internal ids stay internal, and the directory stays private.
- **Former members keep a name.** A lookup by member id returns a removed member's display fields and `status: removed`, and nothing else, so clients can show "Alice (former member)" on contributions that outlive her. A deleted account comes back as a deleted user.
- **Account deletion is refused while the person is the last `creator_role` holder, or the last member, of an org** (**409**, listing the orgs), as on GitHub. The person adds someone with `creator_role` or deletes those orgs first.
