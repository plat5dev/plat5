# Deferred

## Acting on another member

The gateway catalog will not publish a route where the client names a member id. Identity still serves `GET` / `PATCH` / `DELETE /members/{member_id}` on the public port. A member acts on itself through `member` scope, which fills `subject.member_id`.

Service-account keys are not this. They are published under `service_account_id` on organization scope. The member id stays off the client path.

Ready: a check that the resource member belongs to the credential's org, without returning `user_id` to the gateway, and without the gateway calling the public member route through itself. Until then, do not publish that path.
