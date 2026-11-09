//! What a revision serves: the gateway's and identity's feed bodies, built
//! from a checked config with defaults filled in and routes expanded
//! (docs/reference/api/internal.md#control-plane-feeds). Built at apply and
//! stored with the revision; the feeds serve them as stored, and the plan
//! compares them.

use serde_json::{json, Map, Value};

use crate::config::Config;
use crate::expand;

/// A revision's two feed bodies, without their revision.
#[derive(Clone, Debug, PartialEq)]
pub struct Served {
    pub gateway: Value,
    pub identity: Value,
}

impl Served {
    /// The bodies the running version serves for `config`.
    pub fn of(config: &Config) -> Self {
        Self {
            gateway: gateway_body(config),
            identity: identity_body(config),
        }
    }
}

/// A stored body as a feed serves it: `revision` first, then the body.
pub fn feed_body(revision: i64, body: &Value) -> Value {
    let mut out = Map::new();
    out.insert("revision".into(), json!(revision));
    if let Value::Object(fields) = body {
        out.extend(
            fields
                .iter()
                .filter(|(k, _)| *k != "revision")
                .map(|(k, v)| (k.clone(), v.clone())),
        );
    }
    Value::Object(out)
}

/// The body of `GET /internal/gateway`: kinds, every service's routes
/// expanded, rate limits, `defaults.rate_limit`, CORS, `users` (the IdP name
/// claims) and `route_checks`, with defaults filled in. `defaults` carries only
/// `rate_limit`, the part the gateway reads: each service carries its own
/// timeouts and body limit, filled in.
pub fn gateway_body(c: &Config) -> Value {
    json!({
        "kinds": c.kinds,
        "services": expand::expand(c),
        "rate_limits": c.rate_limits,
        "defaults": { "rate_limit": c.defaults.rate_limit },
        "cors": c.cors,
        "users": c.users,
        "route_checks": c.route_checks,
    })
}

/// The body of `GET /internal/identity`: the settings, kinds and roles, with
/// defaults filled in. Roles as written, with their includes.
pub fn identity_body(c: &Config) -> Value {
    json!({
        "deletion_grace": c.deletion_grace,
        "reserved_slugs": c.reserved_slugs,
        "users": c.users,
        "kinds": c.kinds,
        "roles": c.roles,
        "creator_role": c.creator_role,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{apply, fixtures};

    fn base() -> Value {
        fixtures::yaml(&fixtures::dir().join("plans/first-apply/after.yml"))
    }

    fn gateway_feed(revision: i64, config: &Value) -> Value {
        feed_body(revision, &gateway_body(&apply::check(config).unwrap()))
    }

    fn identity_feed(revision: i64, config: &Value) -> Value {
        feed_body(revision, &identity_body(&apply::check(config).unwrap()))
    }

    #[test]
    fn feeds_fill_defaults_and_expand_routes() {
        let gateway = gateway_feed(4, &base());
        assert_eq!(gateway["revision"], json!(4));
        assert_eq!(
            gateway["cors"],
            json!({ "allowed_origins": [], "allowed_headers": [] })
        );
        assert_eq!(
            gateway["rate_limits"],
            json!({ "standard": { "requests": 60, "window_seconds": 60 } })
        );
        assert_eq!(gateway["defaults"], json!({ "rate_limit": ["standard"] }));
        let users = json!({ "name": { "claims": ["name", "preferred_username"], "sync": "first_sign_in" } });
        assert_eq!(gateway["users"], users);
        assert_eq!(gateway["route_checks"], json!({ "path": "/access/routes" }));
        let e10s = &gateway["services"]["e10s"];
        assert_eq!(e10s["timeouts"], json!({ "connect": "10s", "read": "30s" }));
        assert_eq!(e10s["max_body_bytes"], json!(10485760));
        assert_eq!(
            e10s["routes"][2],
            json!({
                "path": "/orgs/{organization}/projects/{project}/plans", "method": "GET",
                "scope": "project", "upstream": "/projects/{project.id}/plans",
                "required_permissions": ["project:read"], "rate_limit": ["standard"], "audit": false,
                "allow_deleted": false
            })
        );
        assert_eq!(e10s["routes"][0]["required_permissions"], Value::Null);
        let identity = identity_feed(4, &base());
        assert_eq!(identity["deletion_grace"], json!("30d"));
        assert_eq!(identity["reserved_slugs"], json!([]));
        assert_eq!(identity["users"], users);
        assert!(identity.get("services").is_none());
        assert!(identity.get("route_checks").is_none());
    }

    #[test]
    fn users_name_fills_each_field_on_its_own() {
        let mut config = base();
        config["users"] = json!({ "name": { "sync": "each_sign_in" } });
        let want =
            json!({ "name": { "claims": ["name", "preferred_username"], "sync": "each_sign_in" } });
        assert_eq!(gateway_feed(1, &config)["users"], want);
        assert_eq!(identity_feed(1, &config)["users"], want);
        config["users"] = json!({ "name": { "claims": ["profile.name"] } });
        let want = json!({ "name": { "claims": ["profile.name"], "sync": "first_sign_in" } });
        assert_eq!(identity_feed(1, &config)["users"], want);
    }
}
