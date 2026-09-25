# Deferred

## Session grants

Member session validate returns `scopes: null`. Mint takes no body and does not accept scopes. A user JWT and a user API key are the same proof for mint. Identity does not copy either credential's labels onto the session.

Ready: a grant model exists for what a user credential may do inside an org, and session mint plus validate follow that model. Until then, do not add a `scopes` column, a mint field, or inheritance from a user API key.

## Acting on another member

The gateway catalog will not publish a route where the client names a member id. Identity still serves `GET` / `PATCH` / `DELETE /members/{member_id}` on the public port. A member acts on itself through `member` scope, which fills `subject.member_id`.

Ready: a check that the resource member belongs to the credential's org, without returning `user_id` to the gateway, and without the gateway calling the public member route through itself. Until then, do not publish that path.
