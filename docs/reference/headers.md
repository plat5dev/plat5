# Headers

Every `X-Plat5-*` header, who sets it and who reads it. What a service should do with them: [write a service](../guides/write-a-service.md).

## From the gateway to services

The same values the upstream placeholders use, so a service can read them either way.

| Header | On | Value |
|--------|----|-------|
| `X-Plat5-<Kind>-Id` (e.g. `X-Plat5-Organization-Id`, `X-Plat5-Project-Id`) | Kind routes | One per node in the chain |
| `X-Plat5-Member-Id` | Kind routes | The caller's member id in this tree |
| `X-Plat5-User-Id` | User routes | The caller's user id |
| `X-Plat5-Principal` | Kind and user routes | `person` or `service_account` (always `person` on a user route) |
| `X-Request-ID` | Every route | Correlation id, gateway-generated; also on the response |
| `traceparent` | Every route | W3C trace context |

`<Kind>` is the kind name in title case, with `_` becoming `-`: `project` → `Project`, `cost_center` → `Cost-Center`.

Never sent: a principal id, the caller's permissions or role, or a user id on a kind route.

**Recommended use:** store the member id as an owner or author when a feature needs one, and key per-person data by the user id on user routes; never decide access by the caller ([`concepts/model.md`](../concepts/model.md#where-a-field-goes)).

## Stripped

| Header | Direction | Why |
|--------|-----------|-----|
| Every `X-Plat5-*` | Request, before the gateway's own are set | A client can't name a node, a member or a user |
| `Authorization` | Request | Consumed by the gateway; never leaked to services |
| `X-Plat5-Audit-*` | Response | What the service tells audit ([`concepts/audit.md`](../concepts/audit.md#what-a-service-adds)). Stripped on every route, audit on or off |

Clients still send `Authorization` **to the gateway**, and CORS allows browsers to send it.

## From services to the gateway

Set on the upstream response of an audited route; the gateway reads them, then strips them on every route ([audit](../concepts/audit.md#what-a-service-adds)).

| Header | Says |
|--------|------|
| `X-Plat5-Audit-Details` | What changed, as one JSON object |
| `X-Plat5-Audit-Created` | The id a create produced |
| `X-Plat5-Audit-Nodes`, `X-Plat5-Audit-Member` | On a user route, the chain of nodes it changed, and the member in that tree |

## Between Plat5's services

None. Internal calls carry no token: the deployment's network is the boundary ([internal APIs](api/internal.md)).

## Other front doors

Base Plat5 defines no other headers. An operator plane documents its own, such as `X-Plat5-Operator-Id`, in its own repo.
