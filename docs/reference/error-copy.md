# Error copy: `message`

Copy appendix for [`reference/errors.md`](errors.md).

## Locked

Who each field of the envelope is for:

| Field | Owner | Use |
|-------|--------|-----|
| `type` | machine | `invalid_request_error` \| `api_error` |
| `code` | machine | Closed set in [`errors.md`](errors.md). Clients branch on this + HTTP status |
| `message` | human | One sentence. Safe to show in a UI. No regex, no `body/key must match`, no `VALIDATION_ERROR` |
| `request_id` | support | Correlation. Not shown in product UI |
| `details` | machine | Extra context. Optional. Never the only place the human sentence lives |

Clients show `error.message`. `details.fields` is for multi-field 422s; `message` is still a complete sentence (the first or most specific field).

## Writing `message`

- Show `error.message`. Don't build a sentence from `details` or field paths.
- Product UI shows the sentence, not `request_id`, HTTP status, or `code`.
- `message` never includes SQL, stack traces, or bind-error text. Bind failures use a fallback sentence.
- Through the gateway, a missing node and a forbidden one are the same **403** sentence. Never write copy that tells them apart.

## Fallback `message` per `code`

| Code | HTTP | Fallback `message` |
|------|------|-------------------|
| `INVALID_REQUEST` | 400 | Malformed request. |
| `VALIDATION_ERROR` | 422 | That doesn't look right. |
| `UNAUTHORIZED` | 401 | Authentication required. |
| `FORBIDDEN` | 403 | Permission denied, or it may not exist. |
| `NOT_FOUND` | 404 | Resource not found. |
| `CONFLICT` | 409 | That already exists. |
| `PLAN_STALE` | 409 | The deployment changed since this plan was made. Plan again. |
| `PAYLOAD_TOO_LARGE` | 413 | Request body is too large. |
| `RATE_LIMITED` | 429 | Too many requests. Try again in a moment. |
| `INTERNAL_ERROR` | 500 | An unexpected error occurred. |
| `SERVICE_UNAVAILABLE` | 503 | Service temporarily unavailable. |

`NOT_FOUND`, `INTERNAL_ERROR` and `SERVICE_UNAVAILABLE` stay generic on purpose. `RATE_LIMITED` stays this sentence: don't interpolate seconds (`details.retry_after_seconds`, `Retry-After` and `X-RateLimit-*` carry that).

## Identity: specific `message`

`FieldError(path, message)` sets `message` as the product sentence. `details.fields[0]` repeats it.

### Nodes

| When | `message` |
|------|-----------|
| Display name empty | Display name is required. |
| Display name > 128 | Display name is too long. |
| Slug malformed | Use 3 to 63 lowercase letters, digits and hyphens, starting with a letter. |
| Slug reserved | That name is reserved. |
| Slug taken, live or tombstone | That name is taken. |
| Child kind whose parent is another kind | Kind {kind} can't go here. |
| Restore with a deleted parent | Restore the {parent kind} first. |
| Restore past the grace period | This can no longer be restored. |
| Create under a deleted node | Restore the {parent kind} first. |
| Release the slug of a node that isn't a purged root | Only a purged root's slug can be released. |

### Members and bindings

| When | `message` |
|------|-----------|
| Add with no IdP subject | Choose someone to add. |
| Already a member | This person is already a member. |
| Direct binding for someone not in the tree | Add this person to the {root kind} first, or invite them. |
| Already bound on this node | This member already has a role here. |
| Remove a service account's member | Delete the service account to remove it. |

### Roles

| When | `message` |
|------|-----------|
| `role` not in the roles document | That role doesn't exist. |
| `role` not bindable at this kind | That role can't be given on this {kind}. |
| `role` missing | Choose a role. |

All are **422** `VALIDATION_ERROR` with `path` `role`.

### Users

| When | `message` |
|------|-----------|
| Name empty | Name is required. |
| Name > 128 | Name is too long. |
| PATCH with no name | Nothing to update. |
| The person's own name edit under `each_sign_in` | Your name comes from your sign-in provider. |

The last is **409** `CONFLICT` with `{ "field": "name", "value": "each_sign_in" }`; the others **422** with `path` `name`.

### Service accounts

| When | `message` |
|------|-----------|
| Name empty | Name is required. |
| Name > 128 | Name is too long. |
| Create with no `role` | Choose a role. |
| PATCH with no name | Nothing to update. |

### Invites

| When | `message` |
|------|-----------|
| `expires_in` not between `60s` and `30d` | Expiry must be between 60 seconds and 30 days. |
| `max_uses` 0, negative, or a string other than `unlimited` | Max uses must be at least 1, or unlimited. |
| `grants` on an invite below a root | Only an invite to the {root kind} can grant more. |
| No `role` | Choose a role. |
| Redeem of a redeemed invite | This invite has already been used. |
| Redeem of a revoked invite | This invite is no longer valid. |
| Redeem of an expired invite | This invite has expired. |
| Redeem of an invite whose node is deleted | This invite's {kind} was deleted. |

Unknown tokens use the generic **404** `Resource not found.` Redeemed, revoked and expired tokens are **409** `CONFLICT` with `{ "field": "status", "value": "redeemed" | "revoked" | "expired" }`; a token whose node is deleted, with `{ "field": "node", "value": "deleted" }`.

### API keys

| When | `message` |
|------|-----------|
| Name > 128 | Name is too long. |

Internal admit is not product UI. Fallback 422 is enough. So is pagination (`limit`, `starting_after`).

## Control plane

`AppError::validation` puts the validation text in `message`, naming the field. Operator-facing.

## Gateway

### Gateway 401

A 401 on a matched route says what the route takes. The same sentence for no credential, an invalid one, and one of the wrong kind: it names the route's requirement, never the state of the credential sent, so it can't be used to test a key. `details.reason` on a JWT failure is unchanged.

| Route scope | `message` |
|-------------|-----------|
| A kind | Authentication required: a sign-in token or an API key. |
| `user` | Authentication required: a sign-in token or a personal API key. |
| [Route checks](gateway.md#route-checks) | Authentication required: a sign-in token or an API key. |

### Gateway 403

The fallback sentence, `Permission denied, or it may not exist.`, with `details` as [`FORBIDDEN`](errors.md#forbidden) gives it, except for a service account on a user route.

| When | `message` |
|------|-----------|
| A node not found, not in the tree, or no route permission held | Permission denied, or it may not exist. |
| A service account on a user route | Service accounts can't use this route. |

The second names no node, so it tells nothing about one; `details` is `null`.

### Route checks 422

`message` is the first field's; `details.fields` has every one.

| `path` | `message` |
|--------|-----------|
| (empty) | The body must be a JSON object. |
| `checks` | checks must be a list of 1 to 100 checks. |
| `checks[i]` | A check is an object with a method and a path. |
| `checks[i].method` | method must be GET, POST, PUT, PATCH, DELETE or HEAD. |
| `checks[i].path` | path must start with /, be visible ASCII with no ? or #, and be at most 2048 characters. |

Siblings (`cli`, `template-*`): update only if they snapshot exact Plat5 `message` strings.
