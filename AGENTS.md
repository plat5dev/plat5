# Plat5: agent contract

Plat5 is a standalone, thin platform runtime for multi-tenant APIs: gateway, identity, audit, and control plane. Sibling repos (`cli`, `operator`, `auth`, `template-*`) are optional modules or clients built on top of Plat5's documented APIs.

## Where truth lives

- **Hard law (model, invariants, wire & config shapes):** `docs/concepts/`, `schema/`, and `docs/reference/` (`gateway`, `headers`, `env`, `idp`, `config/*`, `api/*`).
- **Opt-in standards for user services:** `docs/reference/{errors,lists,telemetry}.md` and `docs/guides/write-a-service.md` — Plat5's own services follow them; user services can follow them for the documented benefits, never forced.
- **Internals:** `docs/internals/` (storage schemas and module map).
- **Index:** [`docs/README.md`](docs/README.md).

## Working here

1. Read `docs/concepts/model.md` and the relevant concept/reference doc before editing.
2. Don't implement past the contract. If code and contract disagree, fix the wrong one — and propose doc changes first for any principle in `concepts/model.md`.
3. One place per rule or shape; link instead of restating. Describe the current contract, not past shapes or migrations.
4. Keep `plat5` self-contained: don't couple runtime docs or code to optional siblings (`cli`, `operator`, `auth`, `template-*`).
5. Keep each service standalone (own Go module / Cargo crate and `Dockerfile` build context, no shared runtime library across services or repos; see [`docs/internals/module-map.md`](docs/internals/module-map.md#deliberate-boundaries)).
6. Don't commit unless asked.
