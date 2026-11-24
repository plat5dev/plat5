//! What a revision serves: the gateway's and identity's feed bodies, built
//! from a checked config with defaults filled in and routes expanded
//! (docs/reference/api/internal.md#control-plane-feeds). Built at apply and
//! stored with the revision; the feeds serve them as stored, and the plan
//! compares them.

use std::collections::{BTreeMap, BTreeSet};

use serde::Serialize;
use serde_json::{json, Map, Value};

use crate::config::{Config, Required};
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
/// expanded, rate limits, `defaults.rate_limit`, CORS, and optional
/// `authorizers`, with defaults filled in.
pub fn gateway_body(c: &Config) -> Value {
    let mut out = Map::new();
    out.insert("kinds".into(), json!(c.kinds));
    out.insert("services".into(), json!(expand::expand(c)));
    out.insert("rate_limits".into(), json!(c.rate_limits));
    out.insert(
        "defaults".into(),
        json!({ "rate_limit": c.defaults.rate_limit }),
    );
    out.insert("cors".into(), json!(c.cors));
    if !c.authorizers.is_empty() {
        out.insert("authorizers".into(), json!(c.authorizers));
    }
    Value::Object(out)
}

/// One entry in the identity feed's derived permission catalog.
#[derive(Clone, Debug, Serialize, PartialEq)]
struct ServedPermission {
    #[serde(skip_serializing_if = "String::is_empty")]
    description: String,
    bindable_at: Vec<String>,
    custom_role_assignable: bool,
}

/// Derives the deployment's permission catalog from kinds (`list_permission`),
/// service routes (`required_permissions`), predefined roles (excluding `*`),
/// and explicit `permissions:` metadata.
fn permission_catalog(c: &Config) -> BTreeMap<String, ServedPermission> {
    let mut all_keys: BTreeSet<String> = c.permissions.keys().cloned().collect();
    for kind in c.kinds.values() {
        if kind.list_permission != "*" {
            all_keys.insert(kind.list_permission.clone());
        }
    }
    for spec in expand::specs(c) {
        if !c.kinds.contains_key(&spec.scope) {
            continue;
        }
        for verb in &spec.verbs {
            if let Some(req) = &verb.required {
                if let Required::Permissions(perms) = &req.value {
                    for p in perms {
                        if p != "*" {
                            all_keys.insert(p.clone());
                        }
                    }
                }
            }
        }
    }
    for role in c.roles.values() {
        for p in &role.permissions {
            if p != "*" {
                all_keys.insert(p.clone());
            }
        }
    }

    let mut out = BTreeMap::new();
    for perm in all_keys {
        let (description, bindable_at, custom_role_assignable) = match c.permissions.get(&perm) {
            Some(meta) => {
                let mut bindable = meta.bindable_at.clone();
                bindable.sort();
                (
                    meta.description.clone(),
                    bindable,
                    meta.custom_role_assignable,
                )
            }
            None => (String::new(), Vec::new(), true),
        };
        out.insert(
            perm,
            ServedPermission {
                description,
                bindable_at,
                custom_role_assignable,
            },
        );
    }
    out
}

/// The body of `GET /internal/identity`: settings, kinds and roles, with
/// defaults filled in. Roles as written, with their includes.
pub fn identity_body(c: &Config) -> Value {
    let mut out = Map::new();
    out.insert("deletion_grace".into(), json!(c.deletion_grace));
    out.insert("reserved_slugs".into(), json!(c.reserved_slugs));
    out.insert("kinds".into(), json!(c.kinds));
    out.insert("roles".into(), json!(c.roles));
    out.insert("creator_role".into(), json!(c.creator_role));
    if c.custom_roles {
        out.insert("custom_roles".into(), json!(true));
    }
    if c.custom_roles || !c.permissions.is_empty() {
        out.insert("permissions".into(), json!(permission_catalog(c)));
    }
    if !c.authorizers.is_empty() {
        out.insert("authorizers".into(), json!(c.authorizers));
    }
    Value::Object(out)
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
            json!({ "standard": { "requests": 60, "window": "60s" } })
        );
        assert_eq!(gateway["defaults"], json!({ "rate_limit": ["standard"] }));
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
        assert!(identity.get("services").is_none());
        assert!(identity.get("custom_roles").is_none());
        assert!(identity.get("permissions").is_none());

        let mut custom = base();
        custom["custom_roles"] = json!(true);
        custom["permissions"] = json!({
            "project:read": { "description": "Read projects" },
            "internal:secret": { "custom_role_assignable": false }
        });
        let identity = identity_feed(5, &custom);
        assert_eq!(identity["custom_roles"], json!(true));
        assert_eq!(
            identity["permissions"]["project:read"],
            json!({ "description": "Read projects", "bindable_at": [], "custom_role_assignable": true })
        );
        assert_eq!(
            identity["permissions"]["internal:secret"],
            json!({ "bindable_at": [], "custom_role_assignable": false })
        );
        assert!(identity["permissions"].get("organization:read").is_some());
        assert!(identity["permissions"].get("*").is_none());

        // Authorizers expand into both feeds with defaults, and inherit service -> route -> verb (with false opt-out).
        let mut authz = base();
        authz["authorizers"] = json!({
            "topaz": { "url": "http://topaz:8383", "token_env": "TOPAZ_API_KEY" }
        });
        authz["kinds"]["project"]["authorizer"] = json!("topaz");
        authz["services"]["e10s"]["authorizer"] = json!({ "name": "topaz", "mode": "or" });
        authz["services"]["e10s"]["scopes"]["project"]["routes"][0]["methods"]["POST"]
            ["authorizer"] = json!(false);
        let gw = gateway_feed(6, &authz);
        assert_eq!(
            gw["authorizers"]["topaz"],
            json!({
                "url": "http://topaz:8383",
                "timeout": "2s",
                "cache_ttl": "0s",
                "negative_cache_ttl": "0s",
                "token_env": "TOPAZ_API_KEY"
            })
        );
        let id = identity_feed(6, &authz);
        assert_eq!(id["authorizers"], gw["authorizers"]);
        assert_eq!(
            id["kinds"]["project"]["authorizer"],
            json!({ "name": "topaz", "mode": "replace" })
        );
        let routes = gw["services"]["e10s"]["routes"].as_array().unwrap();
        let get_plans = routes
            .iter()
            .find(|r| r["method"] == "GET" && r["scope"] == "project")
            .unwrap();
        assert_eq!(
            get_plans["authorizer"],
            json!({ "name": "topaz", "mode": "or" })
        );
        let post_plans = routes
            .iter()
            .find(|r| r["method"] == "POST" && r["scope"] == "project")
            .unwrap();
        assert!(post_plans.get("authorizer").is_none());
    }
}
