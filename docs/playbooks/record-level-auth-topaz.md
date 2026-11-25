# Playbook: External authorization with Topaz (AuthZEN 1.0)

How to pair Plat5's multi-tenant runtime with [Topaz](https://www.topaz.sh/) (or any OpenID AuthZEN 1.0 PDP such as Cerbos or OpenFGA with an AuthZEN adapter) for ReBAC/ABAC — **without** losing zero-auth services, uniform edge **403**s, node list filtering, or `POST /access/routes`.

Authorization in Plat5 is all-or-nothing: a deployment uses either built-in RBAC (`roles` + `creator_role`) or a single top-level `authorizer`. When `authorizer` is set, the PDP decides every permission-gated route, node list filter (`list_permission`), and identity `/access/v1/*` check across all kinds, while Plat5 still handles authentication, tree membership, org security policies, suspension, credential scope narrowing, rate limits, and audit.

## 1. Topaz manifest (`topaz/manifest.yaml`)

Define types matching Plat5's AuthZEN payloads (`member` subjects, node kinds, and your record types). See [`e2e/topaz/manifest.yaml`](../../e2e/topaz/manifest.yaml) for the tested reference:

```yaml
model:
  version: 3

types:
  member: {}

  organization:
    relations:
      admin: member
      viewer: member
    permissions:
      organization:read: viewer | admin
      organization:write: admin
      organization:delete: admin
      organization:bindings:read: admin
      organization:bindings:write: admin
      project:create: admin

  project:
    relations:
      viewer: member
      editor: member
    permissions:
      project:read: viewer | editor
      project:write: editor

  document:
    relations:
      owner: member
      editor: member
      viewer: member
    permissions:
      docs:read: viewer | editor | owner
      docs:write: editor | owner
      docs:delete: owner
```

Run Topaz on your `plat5` Docker network exposing its AuthZEN HTTP gateway on `:8383` and its Directory API on `:9393` (see [`e2e/compose.topaz.yml`](../../e2e/compose.topaz.yml) and [`e2e/topaz/config.yaml`](../../e2e/topaz/config.yaml)). If Topaz requires an API key, set `TOPAZ_API_KEY` on both `gateway` and `identity`.

## 2. Plat5 config (`plat5.yml` & `services/docs.yml`)

Declare top-level `authorizer:` in `plat5.yml` instead of `roles` / `creator_role` ([authorizer reference](../reference/config/authorizer.md)):

```yaml
kinds:
  organization:
    path: /orgs/{organization}
  project:
    parent: organization
    path: /projects/{project}
    bind_creator: false

authorizer:
  url: http://topaz:8383
  timeout: 2s
  cache_ttl: 5s
  negative_cache_ttl: 2s
  # token_env: TOPAZ_API_KEY   # optional bearer token read from gateway/identity env
```

In `services/docs.yml`, declare route `permissions` as usual, and add a flat `resource: { type, id }` on routes that check a record instead of the node:

```yaml
docs:
  url: http://docs:3000
  scopes:
    project:
      upstream_prefix: /projects/{project.id}
      routes:
        # Collection: checked against resource { type: "project", id: "<project ulid>" }
        - path: /documents
          methods:
            GET:  { permissions: project:read }
            POST: { permissions: project:write }

        # Record-level routes: checked against resource { type: "document", id: "<project ulid>:<doc_id>" }
        - path: /documents/{doc_id}
          methods:
            GET:
              permissions: docs:read
              resource: { type: document, id: "{project.id}:{doc_id}" }
            PATCH:
              permissions: docs:write
              resource: { type: document, id: "{project.id}:{doc_id}" }
            DELETE:
              permissions: docs:delete
              resource: { type: document, id: "{project.id}:{doc_id}" }
```

Using `{project.id}:{doc_id}` (or globally unique ULIDs) as `resource.id` guarantees tenant-scoped object IDs inside Topaz.

Identity automatically queries Topaz on node lists (`GET /orgs/{org}/projects` using each kind's `list_permission`) and `/access/v1/*` calls. In external `authorizer` mode, creating a root/member/invite/service account creates the `member` without requiring a `role` (and built-in RBAC writes to `/bindings`, `/groups`, and custom `/roles` return **422**).

## 3. What your service does

Your `docs` service still does **no auth checks** on individual document endpoints — the gateway already enforced Topaz before forwarding. Your service only:

1. **Writes relations to Topaz on create/share/unshare** using `X-Plat5-Member-Id` and the document ID:
   - On `POST /projects/{project_id}/documents`: create the document in your DB and write `(subject: member:<X-Plat5-Member-Id>, relation: owner, object: document:<project_id>:<doc_id>)` to Topaz's directory.
   - On `POST /projects/{project_id}/documents/{doc_id}/shares`: write/delete the `viewer`/`editor` relation in Topaz.
2. **Tunes cache TTLs for freshness vs latency:** keep `cache_ttl: 0s` and `negative_cache_ttl: 0s` (the default) when share/unshare changes must take effect immediately, or set short TTLs (`2s`–`5s`) for high-traffic read paths.
3. **Cleans up tuples on lifecycle events:** when consuming `node.purged` (`kind: project`) or `member.removed` from identity's lifecycle log, delete the corresponding objects/relations in Topaz.

## 4. UI button visibility (`POST /access/routes`)

Frontends don't talk to Topaz either. Pass the concrete document paths to `POST /access/routes`:

```json
POST /access/routes
{
  "checks": [
    { "method": "PATCH",  "path": "/orgs/acme/projects/web/documents/doc-42" },
    { "method": "DELETE", "path": "/orgs/acme/projects/web/documents/doc-42" }
  ]
}
```

The gateway batches all checks into a single AuthZEN `POST /access/v1/evaluations` call and returns `{ "checks": [{ "allowed": true }, { "allowed": false }] }`.
