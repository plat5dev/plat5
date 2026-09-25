# Identity refactor

Identity has landed. [`identity.md`](identity.md) is the contract.

Gateway scopes, header injection, path rewrite, [`gateway-contract.md`](gateway-contract.md), the gateway half of [`identity-boundary.md`](identity-boundary.md), [`routes.md`](routes.md), and `services/identity/routes.yml` still describe the previous paths. Leave them until the gateway pass. Applying `routes.yml` before that pass will not work.

Member sessions have landed (`POST /users/{user_id}/organizations/{organization_id}/session`, `POST /internal/member-sessions/validate`). Do not add that path to `routes.yml` until the gateway admits the session prefix. The gateway does not read `MEMBER_SESSION_VALIDATE_URL`.
