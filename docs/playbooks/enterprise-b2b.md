# Playbook: Enterprise B2B SaaS

A complete configuration pattern for multi-tenant B2B products moving upmarket: `organization → project → environment`, customer-defined custom roles, per-org SSO/MFA and IP guardrails, scoped keys, reversible member suspension, JIT bindings, and plan-tiered rate limits.

Every control lives in Plat5's config and identity API — backend services still do zero auth.

## 1. Gateway environment

Forward the JWT claims your IdP puts on SSO/MFA sessions and scoped OAuth tokens ([env](../reference/env.md#gateway)):

```bash
AUTH_ISSUER=https://idp.example.com
AUTH_JWKS_URI=https://idp.example.com/.well-known/jwks.json
AUTH_ALLOWED_AUDIENCES=https://api.example.com
AUTH_SUBJECT_CLAIM=sub
AUTH_NAME_CLAIMS=name,preferred_username

# Intersect JWT scopes with the caller's member permissions
AUTH_SCOPE_CLAIM=scope

# Trust your edge proxy so org/key IP allowlists read the real client IP
TRUSTED_PROXIES=10.0.0.0/8
```

## 2. Deployment config (`plat5.yml`)

Declare `policy_claims`, enable `custom_roles`, describe assignable permissions in `permissions`, and define plan `tiers` on rate-limit policies:

```yaml
deletion_grace: 30d
reserved_slugs: [new, settings, api, admin]
# Forward IdP claims to admit so orgs can enforce SSO / MFA policies
policy_claims: [amr, idp_connection]

kinds:
  organization:
    path: /orgs/{organization}
    list_permission: organization:read
  project:
    parent: organization
    path: /projects/{project}
    list_permission: project:read
    bind_creator: false
  environment:
    parent: project
    path: /envs/{environment}
    list_permission: project:read
    bind_creator: false

roles:
  admin:
    permissions: ["*"]
    bindable_at: [organization, project, environment]
  member:
    permissions: [organization:read]
    bindable_at: [organization]
  developer:
    includes: [member]
    permissions: [project:read, project:write, project:bindings:read]
    bindable_at: [organization, project, environment]
  viewer:
    includes: [member]
    permissions: [project:read]
    bindable_at: [organization, project, environment]
creator_role: admin

# Let each organization create its own roles from the catalog below.
custom_roles: true
permissions:
  project:read:
    description: View projects, environments, and deployments
    bindable_at: [organization, project, environment]
  project:write:
    description: Create and update deployments
    bindable_at: [organization, project, environment]
  audit:read:
    description: Read organization and project audit logs
    bindable_at: [organization, project]
  # Keep privilege-bearing permissions out of custom roles if desired
  organization:bindings:write:
    custom_role_assignable: false

rate_limits:
  api-org:
    requests: 60
    window: 1s
    count_at: organization
    tiers:
      pro:        { requests: 300,  window: 1s }
      enterprise: { requests: 2000, window: 1s }
  standard:
    requests: 120
    window: 60s

defaults:
  rate_limit: [standard]

services:
  - services/identity.yml
  - services/audit.yml
  - services/api.yml
```

Regenerate `services/identity.yml` so the org-level routes for `/roles`, `/permissions`, `/security-policy`, `/groups`, and `/members/{member_id}/status` are present ([generated routes](../reference/config/generated-routes.md)).

## 3. Per-org enterprise workflows

### Lock down an organization's auth & network policy

An org admin configures the root's security policy ([identity API](../reference/api/identity.md#security-policy)):

```bash
PATCH /orgs/acme/security-policy
{
  "allow_personal_keys": false,
  "allowed_cidrs": ["198.51.100.0/24", "203.0.113.10"],
  "required_idp_claims": {
    "idp_connection": ["okta-acme"],
    "amr": ["sso", "mfa"]
  }
}
```

- Personal API keys are immediately rejected in `acme` (org-owned service accounts still work).
- Any JWT whose `idp_connection` isn't `okta-acme` or whose `amr` doesn't match is refused with **403** in `acme`, while still working normally in other orgs.
- Requests from outside `allowed_cidrs` are refused at the gateway with **403**.

### Let an org create custom roles & bind multiple roles

1. Populate a role builder UI from `GET /orgs/acme/permissions`.
2. Create a custom role:
   ```bash
   POST /orgs/acme/roles
   {
     "display_name": "Compliance Auditor",
     "description": "Read-only access to projects and audit logs",
     "permissions": ["project:read", "audit:read"],
     "bindable_at": ["organization", "project"]
   }
   ```
3. Bind the returned ULID `role.id` alongside another role on the same node — members and groups can hold multiple distinct roles per node ([bindings](../reference/api/identity.md#bindings)).

### Grant time-boxed (JIT) access

Pass `expires_in` (or an RFC 3339 `expires_at`) when binding a member or group:

```bash
POST /orgs/acme/projects/prod/bindings
{
  "member_id": "01jby…",
  "role": "developer",
  "expires_in": "4h"
}
```

The gateway automatically caps its admit cache entry at `expires_at`. When the binding expires, access stops without removing the member row.

### Reversible suspension (for leave-of-absence or SCIM `active: false`)

Suspend a member inside one org without rotating their `member_id` or deleting bindings ([members](../reference/api/identity.md#members)):

```bash
PATCH /orgs/acme/members/{member_id}/status
{ "status": "suspended" }
```

Reactivate later with `{ "status": "active" }`. Changes reach every gateway replica within about a second via the admit-invalidation feed.

### Narrowed API keys with TTLs

```bash
POST /user/keys
{
  "name": "CI read-only deploy check",
  "allowed_roots": ["01jbw…"],
  "scopes": ["project:read"],
  "allowed_cidrs": ["203.0.113.0/24"],
  "expires_in": "30d"
}
```

Narrowed personal keys cannot call user routes (**403**) and can never exceed the intersection of the member's bindings and `scopes`.

### Set an org's rate-limit tier

From an internal service or operator plane on the private network (not exposed on customer routes), tag the root node with a tier name defined in your `rate_limits` policies ([nodes](../reference/api/identity.md#nodes)):

```bash
PATCH {IDENTITY_URL}/nodes/{root_id}/rate-limit-tier
{ "rate_limit_tier": "enterprise" }
```

All routes referencing `api-org` immediately switch from the base `60/1s` budget to `enterprise`'s `2000/1s` budget.
