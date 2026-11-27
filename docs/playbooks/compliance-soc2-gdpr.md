# Playbook: SOC 2, ISO 27001, and GDPR

Compliance belongs to the organization operating a product, not to a library or runtime. No platform can hand you a SOC 2 report or make you GDPR-compliant on its own—roughly half of any audit is organizational (policies, HR onboarding/offboarding, endpoint MDM, vendor reviews, incident drills, penetration tests, and legal DPAs).

What Plat5 gives you out of the box is the **multi-tenant application security architecture** auditors actually inspect: centralized default-deny access enforcement, tenant isolation, per-org SSO/MFA/IP guardrails, fail-closed append-only customer and operator audit logs, declarative change management, and transactional erasure.

This playbook maps Plat5's controls to **SOC 2 Type II**, **ISO/IEC 27001:2022**, and **GDPR**, and spells out the exact baseline your services and deployment must follow to stand behind them in an audit.

---

## 1. Shared responsibility

| Layer | Owned by | What it covers |
|---|---|---|
| **Plat5 runtime + Operator plane** | Plat5 | Edge authentication (JWT / hashed API keys), default-deny RBAC or AuthZEN PDP enforcement, per-org security policies, JIT bindings, sub-second revocation, closed-world tenant ids, fail-closed append-only audit logs (`audit` + `operator-audit`), declarative `plan`/`apply` revision history, transactional lifecycle erasure events, signed multi-arch images with SBOM/provenance |
| **Your services** | Your engineering team | Scoping every database query by the node id from the front door ([write a service](../guides/write-a-service.md#must-do)), never logging secrets/tokens, emitting `X-Plat5-Audit-Details` / `X-Plat5-Audit-Created`, propagating `X-Request-ID` and `traceparent`, consuming `node.purged` / `member.removed` / `user.deleted` to wipe service tables, and checking `/access/v1/evaluations` for background jobs |
| **Deployment & infrastructure** | Your ops / security team | External enterprise IdP (MFA, SSO, lockout), edge TLS termination, internal TLS (`https://`, Postgres `sslmode=require`, Valkey `rediss://`), VPC/network isolation, encryption at rest (KMS/disk), tested encrypted backups, SIEM log shipping & alerting (including 401/429 gateway logs), audit partition archival & retention |
| **Organization & legal** | Your company | Written security policies, employee training & offboarding, laptop MDM, vendor risk management, annual penetration testing, incident response drills, privacy notices, lawful basis / consent records, DPAs, and (if applicable) HIPAA BAAs or PCI scope |

---

## 2. Mandatory compliance baseline

To rely on the control mappings below in an audit, run Plat5 with these settings and conventions:

### Identity & authentication
- **Use an enterprise IdP, not Plat5 Auth.** [Plat5 Auth](https://github.com/plat5dev/auth) is a lightweight reference issuer for local development and simple setups; it does not provide SAML federation, MFA/passkeys, SCIM, or account-lockout policies. In production, point the gateway at a SOC 2 / enterprise IdP (Okta, Auth0, Keycloak, WorkOS, Clerk) and configure `AUTH_ISSUER`, `AUTH_JWKS_URI`, and explicit `AUTH_ALLOWED_AUDIENCES` (never `*`).
- **Enforce org-level MFA/SSO and IP guardrails.** Expose relevant IdP claims via `policy_claims` in `plat5.yml` (e.g., `amr`, `idp_connection`) so enterprise tenants can require SSO/MFA, restrict `allowed_cidrs`, and set `allow_personal_keys: false` ([Enterprise B2B SaaS](enterprise-b2b.md#lock-down-an-organizations-auth--network-policy)).
- **Use a separate IdP for the operator plane.** Never point [Plat5 Operator](https://github.com/plat5dev/operator) at the customer IdP; enforce hardware MFA on internal staff accounts and restrict network reachability to your VPN/tailnet.

### Audit logging & security monitoring
- **Keep `AUDIT_MODE=sync` (the default).** In `sync` mode, the gateway fails closed (`503`) if an intent cannot be recorded, guaranteeing no audited write reaches a service unlogged ([audit](../concepts/audit.md#delivery)). Do not use `AUDIT_MODE=async` or `off` for compliance-scoped deployments.
- **Keep `operator-audit` enabled with role separation.** Run `operator-audit migrate` with the `Owner` Postgres role and `operator-audit serve` with the restricted `Writer` role (no `DELETE`, `TRUNCATE`, or DDL; `serve` refuses to boot otherwise). Every authenticated operator request—**including reads**—is logged fail-closed in a separate database.
- **Ship stdout JSON logs and `/metrics` to a SIEM.** By design, `401`s, `429`s, and not-found `403`s are excluded from `audit_events` to prevent unauthenticated log flooding ([audit](../concepts/audit.md#which-log)). Ship structured gateway and operator stdout logs plus Prometheus metrics (failed-auth limiter counters, `operator_unauthenticated_total`, `audit_writes_total`) to your SIEM (Datadog, Splunk, Grafana Cloud, CloudWatch) with alerting on auth-failure spikes and audit write errors ([telemetry](../reference/telemetry.md)).

### Network & encryption
- **Terminate TLS at the edge and set `TRUSTED_PROXIES`.** Without `TRUSTED_PROXIES`, `X-Forwarded-For` is ignored, breaking both `allowed_cidrs` enforcement and accurate `ip` attribution in audit events ([gateway](../reference/gateway.md#client-ip)).
- **Protect internal traffic.** Keep `INTERNAL_PORT` endpoints strictly on a private VPC/network, enable Postgres TLS (`?sslmode=require` on `DATABASE_URL`) and Valkey TLS (`rediss://` on `VALKEY_URL`), and either use `https://` service URLs / a service mesh or enable Ed25519 request signing (`UPSTREAM_SIGNING_KEY`) for cryptographic upstream verification ([headers](../reference/headers.md#upstream-request-signing), [deploy](../guides/deploy.md#transport-encryption-and-supply-chain-verification)).
- **Encrypt storage and backups at rest.** Run Postgres on encrypted volumes/managed RDS with KMS encryption and automated, periodically restore-tested backups.

### Change management & supply chain
- **Use OIDC JWT auth on the control plane (`AUTH_CONFIG_FILE`) and omit `ADMIN_TOKEN`.** Configure multi-issuer OIDC in `AUTH_CONFIG_FILE` (validated offline with `plat5 validate --auth-config <file>`): grant **`read`** access to pull-request workflows (`plat5 plan --detailed-exitcode`) and **`apply`** access only to protected `main`/release pipelines (keyless GitHub/GitLab Actions OIDC) or MFA-verified human admins via `plat5 login` ([control plane API](../reference/api/control-plane.md#auth)). Every applied revision records the cryptographically verified `actor` (`issuer`, `issuer_name`, `subject`) plus optional `X-Plat5-Change-Reason` / `X-Plat5-Change-Ref` metadata in `GET /revisions`.
- **Pin releases and verify images.** Pin `PLAT5_VERSION` (and `OPERATOR_VERSION`) to immutable release tags or digests—never `latest`. Verify published GHCR images with Cosign and archive BuildKit SBOM / SLSA provenance attestations as part of vendor/supply-chain evidence ([deploy](../guides/deploy.md#transport-encryption-and-supply-chain-verification)).

### Service standards
Every backend service behind Plat5 must follow the three required rules in [write a service](../guides/write-a-service.md#must-do) plus these compliance-relevant standards:
- **Tenant scoping:** Scope every SQL query/mutation by the node id from the path/`X-Plat5-<Kind>-Id` header; never look up records by record id alone.
- **Audit enrichment:** Return `X-Plat5-Audit-Details` (before/after diffs, never secrets) and `X-Plat5-Audit-Created` on writes.
- **Correlation:** Log `X-Request-ID` as `request_id` and propagate `traceparent` so any audit event ties back to gateway and service logs.
- **Lifecycle cleanup:** Consume `GET /internal/events` (`node.purged`, `member.removed`, `user.deleted`) with the cursor stored in the same database as the cleaned data ([lifecycle](../concepts/lifecycle.md#following-the-log)).
- **Background authorization:** Ask `POST /access/v1/evaluations` before sending emails, webhooks, digests, or queued jobs on a member's behalf ([access](../concepts/access.md#outside-a-request)).

---

## 3. SOC 2 & ISO 27001 control mapping

| SOC 2 TSC | ISO 27001:2022 | Control objective | How Plat5 satisfies the platform layer | What your deployment still owns |
|---|---|---|---|---|
| **CC6.1** | **A.5.15, A.8.3, A.8.5** | Logical access security & strong authentication | Centralized edge JWT/API-key verification; default-deny below root; uniform `403` prevents tenant/node enumeration; per-org `required_idp_claims` (SSO/MFA) and `allowed_cidrs`; hashed keys/invites (`key_hash`, `token_hash`) with leak-scanner prefixes | External IdP MFA/password/lockout policies; operator IdP MFA; private network / VPN |
| **CC6.2, CC6.3** | **A.5.18, A.8.2** | Provisioning, least privilege, and timely revocation | Role-based permissions (`custom_role_assignable: false` prevents privilege escalation; custom roles cannot grant `*`) or external AuthZEN PDP; scoped/expiring API keys (`scopes`, `allowed_roots`, `allowed_cidrs`, `expires_at`); JIT expiring bindings (`expires_in`/`expires_at`); reversible member suspension; sub-second revocation across gateway replicas via `admit-invalidations` | Periodic user & operator access reviews; HR offboarding process (calling suspend/remove or SCIM) |
| **CC6.6, CC6.7** | **A.8.20, A.8.24** | Boundary protection & data in transit | Security headers (`nosniff`, `DENY`, `Referrer-Policy`); strict CORS allowlists; request body limits (`max_body_bytes`); path-traversal rejection; failed-auth IP rate limiter; optional Ed25519 `X-Plat5-Signature`; supports `https://` upstreams, Postgres TLS, and Valkey `rediss://` | Edge TLS certificates; VPC security groups / egress SSRF rules; DB encryption at rest (KMS) |
| **CC7.1, CC7.2** | **A.8.15, A.8.16** | Monitoring, anomaly detection, and tamper-resistant audit trails | Fail-closed `AUDIT_MODE=sync` before upstream forward; append-only Postgres `audit_events` (DB trigger, no edit/delete API); separate immutable `operator-audit` logging every operator read & write (including `acting_as` impersonation) with `Owner`/`Writer` DB role separation; `AUDIT_MODE` and `AUTH_ISSUER` isolated from `ADMIN_TOKEN`; correlated `X-Request-ID`, W3C traces, and `/metrics` | SIEM ingestion, alerting rules (401/429 spikes, audit failures), WORM archival, and incident response |
| **CC8.1** | **A.8.32** | Change management for configuration & releases | Declarative `plat5 plan` / `plat5 apply` with dry-run diffs, strict JSON Schema validation, optimistic preconditions (`expect.revision` / `409 PLAN_STALE`), blocked destructive kind changes, multi-issuer OIDC `read`/`apply` separation (`AUTH_CONFIG_FILE`), immutable Postgres revision history with cryptographically verified `actor` + `reason`/`ref` metadata (`GET /revisions`), CI drift detection (`--detailed-exitcode`), and signed multi-arch images with SBOM + SLSA provenance | Git branch protection, PR approvals, and application release approvals |
| **A1.2** *(Availability)* | **A.8.13, A.8.14** | Resilience and recovery | Stateless horizontal gateway/identity/audit replicas; gateway serves last snapshot during control-plane outages and fails open if Valkey is unreachable; `/health/live` & `/health/ready` probes; replayable lifecycle log reconciles restored service backups | Multi-AZ hosting, automated Postgres backups, and periodic backup-restore drills |

---

## 4. GDPR alignment

### Data minimization & privacy by design (Art. 5(1)(c), Art. 25, Art. 32)
- **Strict identity separation:** `principal_id` never leaves `identity`. Tree routes and tree audit logs receive only tree-scoped opaque `member_id`s—never `user_id` or `principal_id`. User routes receive `user_id` with no tree context ([model](../concepts/model.md#principles)).
- **Minimal audit payloads:** Audit events never record request bodies, response bodies, query strings, credentials, roles, permissions, or slugs ([audit](../concepts/audit.md#not-here)). Services must follow the same rule in `X-Plat5-Audit-Details`.
- **Regional deployment:** Each Plat5 stack is self-contained (one Postgres, one tree namespace), making EU-only regional deployments straightforward when data residency is required.

### Right to erasure & tenant offboarding (Art. 17)
- **Account deletion (`DELETE /users/{user_id}`):** Runs in a single transaction—removes all tree memberships (deleting direct bindings and group memberships), deletes personal API keys and IdP links, deletes the principal row, strips the display name from historical removed member rows, and writes commit-ordered `member.removed` and `user.deleted` lifecycle events ([lifecycle](../concepts/lifecycle.md#deleting-an-account)).
- **Tenant offboarding:** Deleting a root node starts the configured `deletion_grace` window; once elapsed, the purge sweeper marks the tree `purged`, deletes bindings, invites, groups, and service accounts/keys, removes members, and emits `node.purged` ([lifecycle](../concepts/lifecycle.md#deleting-restoring-and-purging)).
- **Backup-safe service cleanup:** Because services keep their lifecycle cursor in the same database as the data they delete, restoring a service database from an older backup automatically rewinds the cursor and replays any `user.deleted`, `member.removed`, or `node.purged` events that happened after the backup—preventing "zombie" deleted data from reappearing after disaster recovery ([lifecycle](../concepts/lifecycle.md#following-the-log)).

### Right of access & data portability / DSAR (Art. 15, Art. 20)
Plat5 exposes the personal data it holds for a person through existing user-scoped routes, which your DSAR workflow (or self-service export handler) can bundle alongside your own services' `user_id`-keyed records:

- `GET /user` — person profile (`user_id`, display name, status)
- `GET /user/nodes` — organizations / trees the person belongs to
- `GET /user/keys` — personal API key metadata (prefixes, names, scopes, expiry; raw secrets are never stored)
- `GET /user/audit-events` — the person's own user-route audit log

### Storage limitation & audit log retention (Art. 5(1)(e))
Audit events record client `ip`, `user_agent`, and `actor.user_id` (on user routes), which constitute personal data under GDPR.

- **Do not delete individual rows on `user.deleted`.** Under GDPR Art. 6(1)(f) (legitimate security interest) and Art. 17(3)(b)/(e) (legal compliance / defense of legal claims), immutable security audit logs may be retained after account deletion **provided retention is bounded and documented** in your privacy notice and retention schedule. Per-row deletion would undermine SOC 2 / ISO 27001 audit integrity.
- **Enforce retention at the monthly partition boundary.** Both `plat5`'s `audit` and `operator-audit` store `audit_events` in monthly Postgres partitions (`audit.audit_events_YYYY_MM`, plus `audit.audit_log_keys_YYYY_MM` in Plat5 audit; see [audit storage](../internals/audit-storage.md)). Run a scheduled job under the Postgres owner role to archive completed partitions to encrypted WORM storage (e.g., S3 Object Lock) if required for multi-year retention, then drop expired partitions:

```sql
-- Example: drop partitions older than your documented retention window (run as schema owner after archival)
DROP TABLE IF EXISTS audit.audit_log_keys_2025_01;
DROP TABLE IF EXISTS audit.audit_events_2025_01;
```

---

## 5. Known boundaries & operational caveats

Be explicit about these when completing enterprise security questionnaires:

- **Operator plane authorization:** Run [Plat5 Operator](https://github.com/plat5dev/operator) with `authorizer.mode: authzen` backed by an OpenID AuthZEN 1.0 PDP (such as Topaz or Cerbos), set `TRUSTED_PROXIES`, and enforce `X-Plat5-Operator-Reason` in your PDP policy for privileged writes and `X-Plat5-As-Member` impersonation. Reserve `authorizer.mode: disabled` strictly for local development.
- **Control-plane static `ADMIN_TOKEN`:** Omit `ADMIN_TOKEN` in production once `AUTH_CONFIG_FILE` is configured so no shared static root secret exists. If retained for break-glass, store it in an audited secrets vault with MFA-gated checkout.
- **HIPAA & PCI-DSS:** Plat5's controls align well with HIPAA access and audit rules, but HIPAA requires BAAs with every cloud/IdP/logging vendor touching PHI and strict end-to-end encryption; PCI-DSS requires dedicated cardholder-environment segmentation (most teams keep card data out of scope using a payment processor). Treat Plat5 as one component of a regulated deployment, not a pre-certified HIPAA/PCI environment.
