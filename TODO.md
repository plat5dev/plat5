# Deferred

## Session grants

Member session validate returns `scopes: null`. Mint takes no body and does not accept scopes. A user JWT and a user API key are the same proof for mint. Identity does not copy either credential's labels onto the session.

Ready: a grant model exists for what a user credential may do inside an org, and session mint plus validate follow that model. Until then, do not add a `scopes` column, a mint field, or inheritance from a user API key.
