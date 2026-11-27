# Security Policy

## Supported Versions

Security fixes are released for the latest minor version line of Plat5 (`v*` release tags). Pin deployments to an explicit release tag or image digest and stay current with patch releases.

## Reporting a Vulnerability

**Do not open a public GitHub issue for security vulnerabilities.**

Please report vulnerabilities privately using [GitHub Private Vulnerability Reporting](https://github.com/plat5dev/plat5/security/advisories/new).

Include as much detail as possible to help reproduce and assess the issue:

- Affected component (`gateway`, `control-plane`, `identity`, `audit`) and version/tag
- Step-by-step reproduction instructions or proof of concept
- Potential impact and any suggested mitigation

## Response Process

- We aim to acknowledge private reports within 3 business days.
- Once validated, we will work on a fix and coordinate disclosure with you before publishing a security advisory and patched release.

## Hardening & Compliance

For production hardening, image verification (Cosign / SBOM / SLSA provenance), and SOC 2 / ISO 27001 / GDPR guidance, see:

- [`docs/guides/deploy.md`](docs/guides/deploy.md)
- [`docs/playbooks/compliance-soc2-gdpr.md`](docs/playbooks/compliance-soc2-gdpr.md)
