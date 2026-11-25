# Playbook: Public endpoints, guests, and subdomain routing

How to build products with unauthenticated public endpoints (public status pages, docs, webhooks), external single-project collaborators (guests), and tenant subdomains (`acme.api.example.com`) — all through the same Plat5 gateway.

## 1. Config (`plat5.yml` & `services/portal.yml`)

Enable `host` on the root kind in `plat5.yml`:

```yaml
kinds:
  organization:
    path: /orgs/{organization}
    host: "{organization}.api.example.com"
  project:
    parent: organization
    path: /projects/{project}
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
creator_role: admin
```

Put unauthenticated endpoints under `scopes.public`, and authenticated tenant endpoints under the kind scope in `services/portal.yml`:

```yaml
portal:
  url: http://portal:3000
  scopes:
    public:
      routes:
        - path: /public/roadmaps/{slug}
          methods: [GET]
    project:
      upstream_prefix: /projects/{project.id}
      routes:
        - path: /roadmap
          methods:
            GET:  { permissions: project:read }
            POST: { permissions: project:write }
```

Kind-scoped routes always require an authenticated member of the tree (`X-Plat5-Member-Id` is always present). Truly unauthenticated endpoints use `scope: public`, where the gateway skips auth/admit and your service decides which published records to expose.

## 2. Subdomain routing at your TLS edge

With `host: "{organization}.api.example.com"` on the root kind:
- Clients can call either `GET https://acme.api.example.com/projects/roadmap/roadmap` **or** `GET https://api.example.com/orgs/acme/projects/roadmap/roadmap`.
- Both resolve `acme` to its ULID and enforce cross-tenant isolation: sending `Host: acme.api.example.com` with `/orgs/other-org/...` is rejected so a subdomain can never address another tenant's path.
- Point a wildcard TLS cert (`*.api.example.com`) at your edge proxy (Caddy, nginx, Cloudflare) and preserve the `Host` header when forwarding to the gateway.
- For fully custom white-label domains (`roadmap.acme.com`), map `Host: roadmap.acme.com` → `Host: acme.api.example.com` (or prefix `/orgs/acme`) at your edge proxy.

## 3. Adding external collaborators (guests)

When an org wants to invite an outside contractor to **only one project**:
1. Grant `guests:write` to the role allowed to send project invites (e.g., `admin`, or a `project_manager` role).
2. Create an invite at the project node:
   ```bash
   POST /orgs/acme/projects/roadmap/invites
   { "role": "editor", "expires_in": "7d" }
   ```
3. The invitee redeems the token via `POST /user/invites/redeem` and gets a `member_id` in `acme` marked `guest: true` (visible on `GET /orgs/acme/members`), able to reach only `/projects/roadmap` and nothing else in `acme`.
