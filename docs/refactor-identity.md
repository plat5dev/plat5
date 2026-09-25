# Identity refactor

Identity has landed. [`identity.md`](identity.md) is the contract.

Gateway scopes, header injection, path rewrite, [`gateway-contract.md`](gateway-contract.md), the gateway half of [`identity-boundary.md`](identity-boundary.md), [`routes.md`](routes.md), and `services/identity/routes.yml` still describe the previous paths. Leave them until the gateway pass. Applying `routes.yml` before that pass will not work.
