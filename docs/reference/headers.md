# Headers

Every `X-Plat5-*` header, who sets it and who reads it. What a service should do with them: [write a service](../guides/write-a-service.md).

## From the gateway to services

The same subject the path rewrite uses, so a service can read it either way.

| Header | Value |
|--------|-------|
| `X-Plat5-<Kind>-Id` (e.g. `X-Plat5-Organization-Id`, `X-Plat5-Project-Id`) | One per node in the chain |
| `X-Plat5-Member-Id` | The caller's member id in this tree; on a sign-in route, their directory member |
| `X-Plat5-Principal` | `person` or `service_account` |
| `X-Request-ID` | Correlation id, gateway-generated; also on the response |
| `traceparent` | W3C trace context |

`<Kind>` is the kind name in title case, with `_` becoming `-`: `project` → `Project`, `cost_center` → `Cost-Center`.

Never sent: a person's principal id, the caller's labels or role.

**Recommended use:** store the member id as an owner or author when a feature needs one; never decide access by the caller ([`concepts/model.md`](../concepts/model.md#where-a-field-goes)).

## Stripped

| Header | Direction | Why |
|--------|-----------|-----|
| Every `X-Plat5-*` | Request, before the subject headers are set | A client can't name a node or a member |
| `Authorization`, `X-API-Key` | Request | Consumed by the gateway; never leaked to services |
| `X-Plat5-Audit-*` | Response | What the service tells audit ([`concepts/audit.md`](../concepts/audit.md#what-a-service-adds)). Stripped on every route, audit on or off |

Clients still send credential headers **to the gateway**, and CORS may allow browsers to send them.

## From services to the gateway

Set on the upstream response of an audited route; the gateway reads them, then strips them on every route ([audit](../concepts/audit.md#what-a-service-adds)).

| Header | Says |
|--------|------|
| `X-Plat5-Audit-Details` | What changed, as one JSON object |
| `X-Plat5-Audit-Created` | The id a create produced |
| `X-Plat5-Audit-Root`, `X-Plat5-Audit-Member` | On a sign-in route, the tree it changed and the member in it |

## Between Plat5's services

| Header | Says |
|--------|------|
| `X-Plat5-Internal-Token` | The shared `INTERNAL_AUTH_TOKEN`, on every internal call ([internal APIs](api/internal.md)) |

## Other front doors

Base Plat5 defines no other headers. An operator plane documents its own, such as `X-Plat5-Operator-Id`, in its own repo.
