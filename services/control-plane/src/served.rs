//! What a revision serves: the gateway's and identity's feed bodies, built
//! from a checked config with defaults filled in and routes expanded
//! (docs/reference/api/internal.md#control-plane-feeds). Built at apply and
//! stored with the revision; the feeds serve them as stored, and the plan
//! compares them.

use std::collections::{BTreeMap, BTreeSet};

use serde::Serialize;
use serde_json::{json, Map, Value};

use crate::config::Config;
use crate::expand::ExpandedService;
use crate::rules::Checked;

/// A revision's two feed bodies, without their revision.
#[derive(Clone, Debug, PartialEq)]
pub struct Served {
    pub gateway: Value,
    pub identity: Value,
}

impl Served {
    /// The bodies the running version serves for a checked config (single expansion pass).
    pub fn of_checked(config: &Config, checked: &Checked) -> Self {
        Self {
            gateway: gateway_body_with(config, &checked.services),
            identity: identity_body_with(config, &checked.route_permissions),
        }
    }

    #[cfg(test)]
    pub fn of(config: &Config) -> Self {
        let checked = crate::rules::check(config).expect("checked config");
        Self::of_checked(config, &checked)
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
/// expanded, rate limits, `defaults.rate_limit`, optional `policy_claims`,
/// and optional deployment-wide `authorizer`.
pub fn gateway_body_with(c: &Config, services: &BTreeMap<String, ExpandedService>) -> Value {
    let mut out = Map::new();
    out.insert("kinds".into(), json!(c.kinds));
    out.insert("services".into(), json!(services));
    out.insert("rate_limits".into(), json!(c.rate_limits));
    out.insert(
        "defaults".into(),
        json!({ "rate_limit": c.defaults.rate_limit }),
    );
    if !c.policy_claims.is_empty() {
        let mut claims = c.policy_claims.clone();
        claims.sort();
        out.insert("policy_claims".into(), json!(claims));
    }
    if let Some(authz) = &c.authorizer {
        out.insert("authorizer".into(), json!(authz));
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

/// Slim authorizer projection for identity (excludes gateway-only cache TTLs).
#[derive(Clone, Debug, Serialize, PartialEq)]
struct IdentityAuthorizer<'a> {
    url: &'a str,
    timeout: &'a str,
    #[serde(skip_serializing_if = "Option::is_none")]
    token_env: Option<&'a str>,
}

/// Derives the deployment's permission catalog from kinds (`list_permission`),
/// service routes (`permissions`), predefined roles (excluding `*`),
/// and explicit `permissions:` metadata.
fn permission_catalog(
    c: &Config,
    route_permissions: &BTreeSet<String>,
) -> BTreeMap<String, ServedPermission> {
    let mut all_keys: BTreeSet<String> = c.permissions.keys().cloned().collect();
    for kind in c.kinds.values() {
        all_keys.insert(kind.list_permission.clone());
    }
    all_keys.extend(route_permissions.iter().cloned());
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

/// The body of `GET /internal/identity`: settings, kinds, optional `policy_claims`,
/// and either built-in RBAC (`roles`, `creator_role`, optional `custom_roles` and
/// `permissions` catalog) or the deployment-wide `authorizer`.
pub fn identity_body_with(c: &Config, route_permissions: &BTreeSet<String>) -> Value {
    let mut out = Map::new();
    out.insert("deletion_grace".into(), json!(c.deletion_grace));
    out.insert("reserved_slugs".into(), json!(c.reserved_slugs));
    if !c.policy_claims.is_empty() {
        let mut claims = c.policy_claims.clone();
        claims.sort();
        out.insert("policy_claims".into(), json!(claims));
    }
    out.insert("kinds".into(), json!(c.kinds));
    if let Some(authz) = &c.authorizer {
        out.insert(
            "authorizer".into(),
            json!(IdentityAuthorizer {
                url: &authz.url,
                timeout: &authz.timeout,
                token_env: authz.token_env.as_deref(),
            }),
        );
    } else {
        out.insert("roles".into(), json!(c.roles));
        if let Some(creator_role) = &c.creator_role {
            out.insert("creator_role".into(), json!(creator_role));
        }
        if c.custom_roles {
            out.insert("custom_roles".into(), json!(true));
        }
        if c.custom_roles || !c.permissions.is_empty() {
            out.insert(
                "permissions".into(),
                json!(permission_catalog(c, route_permissions)),
            );
        }
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

    fn served(config: &Value) -> Served {
        Served::of(&apply::check(config).unwrap())
    }

    fn gateway_feed(revision: i64, config: &Value) -> Value {
        feed_body(revision, &served(config).gateway)
    }

    fn identity_feed(revision: i64, config: &Value) -> Value {
        feed_body(revision, &served(config).identity)
    }

    #[test]
    fn feeds_fill_defaults_and_expand_routes() {
        let gateway = gateway_feed(4, &base());
        assert_eq!(gateway["revision"], json!(4));
        assert!(gateway.get("cors").is_none());
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
                "permissions": ["project:read"], "rate_limit": ["standard"], "audit": false,
                "allow_deleted": false
            })
        );
        assert_eq!(e10s["routes"][0]["permissions"], Value::Null);
        let identity = identity_feed(4, &base());
        assert_eq!(identity["deletion_grace"], json!("30d"));
        assert_eq!(identity["reserved_slugs"], json!([]));
        assert!(identity.get("services").is_none());
        assert!(identity.get("custom_roles").is_none());
        assert!(identity.get("permissions").is_none());

        let mut custom = base();
        custom["custom_roles"] = json!(true);
        custom["policy_claims"] = json!(["amr", "acr"]);
        custom["permissions"] = json!({
            "project:read": { "description": "Read projects" },
            "internal:secret": { "custom_role_assignable": false }
        });
        let gw = gateway_feed(5, &custom);
        assert_eq!(gw["policy_claims"], json!(["acr", "amr"]));
        let identity = identity_feed(5, &custom);
        assert_eq!(identity["policy_claims"], json!(["acr", "amr"]));
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

        // External authorizer mode replaces roles/creator_role and sends
        // authorizer to both feeds (slimmed for identity) plus route resource.
        let mut authz = base();
        authz.as_object_mut().unwrap().remove("roles");
        authz.as_object_mut().unwrap().remove("creator_role");
        authz["authorizer"] = json!({
            "url": "http://topaz:8383",
            "cache_ttl": "5s",
            "token_env": "TOPAZ_API_KEY"
        });
        authz["services"]["e10s"]["scopes"]["project"]["routes"][0]["resource"] = json!({
            "type": "plan",
            "id": "{project.id}"
        });
        let gw = gateway_feed(6, &authz);
        assert_eq!(
            gw["authorizer"],
            json!({
                "url": "http://topaz:8383",
                "timeout": "2s",
                "cache_ttl": "5s",
                "negative_cache_ttl": "0s",
                "token_env": "TOPAZ_API_KEY"
            })
        );
        let id = identity_feed(6, &authz);
        assert_eq!(
            id["authorizer"],
            json!({
                "url": "http://topaz:8383",
                "timeout": "2s",
                "token_env": "TOPAZ_API_KEY"
            })
        );
        assert!(id.get("roles").is_none());
        assert!(id.get("creator_role").is_none());
        let routes = gw["services"]["e10s"]["routes"].as_array().unwrap();
        let get_plans = routes
            .iter()
            .find(|r| r["method"] == "GET" && r["scope"] == "project")
            .unwrap();
        assert_eq!(
            get_plans["resource"],
            json!({ "type": "plan", "id": "{project.id}" })
        );
    }
}
