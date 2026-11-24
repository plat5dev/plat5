# Playbook: Record-level auth with Topaz (AuthZEN 1.0)

How to pair Plat5's multi-tenant runtime with [Topaz](https://www.topaz.sh/) (or any OpenID AuthZEN 1.0 PDP such as Cerbos or OpenFGA with an AuthZEN adapter) for fine-grained ReBAC/ABAC — **without** losing zero-auth services, uniform edge **403**s, or `POST /access/routes`.

## When to use each mode

| Mode | Behavior | Best for |
|------|----------|----------|
| `or` | Built-in RBAC runs first; org/project admins bypass the PDP. Otherwise calls Topaz | Additive per-record sharing (e.g., share one document with a colleague who doesn't have project-wide read) |
| `and` | Built-in RBAC must allow first; then Topaz must also allow | Narrowing writes/deletes to record owners/editors, ABAC rules, or explicit denies |
| `replace` | Topaz decides alone (Plat5 still enforces credentials, org security policies, suspension, and credential scopes) | Fully delegating a kind's or service's authorization to Topaz |

## 1. Topaz manifest (`topaz/manifest.yaml`)

Define types matching Plat5's AuthZEN payloads (`member` subjects, node kinds, and your record types). See [`e2e/topaz/manifest.yaml`](../../e2e/topaz/manifest.yaml) for the tested reference:

```yaml
model:
  version: 3

types:
  member: {}

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

Declare `topaz` under `authorizers:` in `plat5.yml` ([authorizers reference](../reference/config/authorizers.md)):

```yaml
authorizers:
  topaz:
    url: http://topaz:8383
    timeout: 2s
    cache_ttl: 5s
    negative_cache_ttl: 2s
    # token_env: TOPAZ_API_KEY   # optional bearer token read from gateway/identity env
```

Attach it to routes in `services/docs.yml`. Notice how `authorizer` inherits from service → scope → route → verb, and how record routes use `resource: { type, id }`:

```yaml
docs:
  url: http://docs:3000
  scopes:
    project:
      upstream_prefix: /projects/{project.id}
      routes:
        # Collection: coarse project permission via Plat5 RBAC alone
        - path: /documents
          methods:
            GET:  { required_permissions: [project:read] }
            POST: { required_permissions: [project:write] }

        # Record-level routes: compose Plat5 RBAC with Topaz on document {doc_id}
        - path: /documents/{doc_id}
          methods:
            GET:
              required_permissions: [project:read]
              authorizer:
                name: topaz
                mode: or          # project readers/admins pass fast; shared viewers pass via Topaz
                resource: { type: document, id: "{project.id}:{doc_id}" }
                action: docs:read
            PATCH:
              required_permissions: [project:write]
              authorizer:
                name: topaz
                mode: and         # must hold project:write AND be document editor/owner in Topaz
                resource: { type: document, id: "{project.id}:{doc_id}" }
                action: docs:write
            DELETE:
              required_permissions: [project:write]
              authorizer:
                name: topaz
                mode: replace     # only Topaz document owner can delete (even org admins must own it)
                resource: { type: document, id: "{project.id}:{doc_id}" }
                action: docs:delete
```

Using `{project.id}:{doc_id}` (or globally unique ULIDs) as `resource.id` guarantees tenant-scoped object IDs inside Topaz.

### Optional: delegate node listing (`list_permission`) to Topaz

If Topaz also owns project-level visibility (for example, `GET /orgs/{org}/projects` and `POST /access/v1/search/resource`), attach `authorizer` to the kind itself in `plat5.yml`:

```yaml
kinds:
  organization:
    path: /orgs/{organization}
  project:
    parent: organization
    path: /projects/{project}
    authorizer:
      name: topaz
      mode: or
```

Identity will batch-evaluate projects against Topaz on child lists and `/access/v1/*` calls, short-circuiting when built-in RBAC already decides the answer.

## 3. What your service does

Your `docs` service still does **no auth checks** on individual document endpoints — the gateway already enforced Topaz before forwarding. Your service only:

1. **Writes relations to Topaz on create/share/unshare** using `X-Plat5-Member-Id` and the document ID:
   - On `POST /projects/{project_id}/documents`: create the document in your DB and write `(subject: member:<X-Plat5-Member-Id>, relation: owner, object: document:<project_id>:<doc_id>)` to Topaz's directory.
   - On `POST /projects/{project_id}/documents/{doc_id}/shares`: write/delete the `viewer`/`editor` relation in Topaz.
2. **Invalidates the gateway cache immediately on permission changes** when `cache_ttl` or `negative_cache_ttl` > `0s` ([gateway internal API](../reference/api/internal.md#authorizer-cache-invalidation)):
   ```bash
   POST {GATEWAY_INTERNAL_URL}/internal/authorizer-cache/invalidate
   {
     "authorizer": "topaz",
     "resource_type": "document",
     "resource_id": "01jbx…:doc-42"
   }
   ```
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

The gateway batches all checks per authorizer into a single AuthZEN `POST /access/v1/evaluations` call and returns `{ "checks": [{ "allowed": true }, { "allowed": false }] }`.
