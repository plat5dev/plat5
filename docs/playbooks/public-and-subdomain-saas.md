# Playbook: Public portals, guests, and subdomain routing

How to build products with public read-only spaces (public roadmaps, status pages, shared docs), external single-project collaborators (guests), and tenant subdomains (`acme.api.example.com`) — all through the same Plat5 gateway routes and slug-to-ULID pipeline.

## 1. Config (`plat5.yml` & `services/portal.yml`)

Enable `host` on the root kind and `allow_public: true` on the kind whose nodes can be published:

```yaml
kinds:
  organization:
    path: /orgs/{organization}
    host: "{organization}.api.example.com"
  project:
    parent: organization
    path: /projects/{project}
    allow_public: true
    bind_creator: false

roles:
  admin:
    permissions: ["*"]
    bindable_at: [organization, project]
  member:
    permissions: [organization:read]
    bindable_at: [organization]
  editor:
    includes: [member]
    permissions: [project:read, project:write]
    bindable_at: [organization, project]
  public_viewer:
    permissions: [project:read]
    bindable_at: [project]
creator_role: admin
```

Opt specific read routes into `allow_public: true` in `services/portal.yml` (write routes stay private):

```yaml
portal:
  url: http://portal:3000
  scopes:
    project:
      upstream_prefix: /projects/{project.id}
      routes:
        - path: /roadmap
          methods:
            GET:
              required_permissions: [project:read]
              allow_public: true
            POST:
              required_permissions: [project:write]
              # allow_public defaults to false: only authenticated members with project:write
```

## 2. Making a project public (or authenticated-only)

By default, every project is private — `allow_public: true` only *permits* public bindings; it grants nothing on its own.

- **Public to the internet (anonymous + signed-in):**
  ```bash
  POST /orgs/acme/projects/roadmap/bindings
  { "public_principal": "all_users", "role": "public_viewer" }
  ```
- **Visible to any signed-in user of your platform (not anonymous):**
  ```bash
  POST /orgs/acme/projects/roadmap/bindings
  { "public_principal": "all_authenticated_users", "role": "public_viewer" }
  ```
- **Make it private again:** delete that binding (`DELETE /orgs/acme/projects/roadmap/bindings/{binding_id}`). Access revokes within a second across all gateway replicas.
- **Suspended members can't bypass suspension:** if a member of `acme` is suspended, public bindings never grant them access to `acme`'s nodes.

## 3. Handling `allow_public` requests in your service

On a normal authenticated request, your `portal` service receives `X-Plat5-Organization-Id`, `X-Plat5-Project-Id`, `X-Plat5-Member-Id`, and `X-Plat5-Principal: person`.

On an `allow_public: true` route accessed anonymously or by a signed-in user outside the org ([headers](../reference/headers.md)):
- `X-Plat5-Organization-Id` and `X-Plat5-Project-Id` are **still resolved and present** (your SQL stays `WHERE project_id = $1`).
- `X-Plat5-Member-Id` is **omitted**.
- `X-Plat5-Principal` is `anonymous` (for unauthenticated callers) or `person` (for a signed-in non-member via `all_authenticated_users`).

So read handlers only require the node ID from the path, and check whether `X-Plat5-Member-Id` is present before personalizing responses (for example, hiding "your votes" when anonymous). Write handlers still require `X-Plat5-Member-Id` and answer **400** if absent.

## 4. Subdomain routing at your TLS edge

With `host: "{organization}.api.example.com"` on the root kind:
- Clients can call either `GET https://acme.api.example.com/projects/roadmap/roadmap` **or** `GET https://api.example.com/orgs/acme/projects/roadmap/roadmap`.
- Both resolve `acme` to its ULID and enforce cross-tenant isolation: sending `Host: acme.api.example.com` with `/orgs/other-org/...` is rejected so a subdomain can never address another tenant's path.
- Point a wildcard TLS cert (`*.api.example.com`) at your edge proxy (Caddy, nginx, Cloudflare) and preserve the `Host` header when forwarding to the gateway.
- For fully custom white-label domains (`roadmap.acme.com`), map `Host: roadmap.acme.com` → `Host: acme.api.example.com` (or prefix `/orgs/acme`) at your edge proxy.

## 5. Adding external collaborators (guests)

When an org wants to invite an outside contractor to **only one project** (with write access, not public):
1. Grant `guests:write` to the role allowed to send project invites (e.g., `admin`, or a `project_manager` role).
2. Create an invite at the project node:
   ```bash
   POST /orgs/acme/projects/roadmap/invites
   { "role": "editor", "expires_in": "7d" }
   ```
3. The invitee redeems the token via `POST /user/invites/redeem` and gets a `member_id` in `acme` marked `guest: true` (visible on `GET /orgs/acme/members`), able to reach only `/projects/roadmap` and nothing else in `acme`.
