//! Every service's routes as the gateway matches them: one per method, with
//! the full public path, the upstream, and every default filled in
//! (docs/reference/api/internal.md#control-plane-feeds). Identity's and audit's
//! are routes like any other service's: their files say what they are.
//!
//! Each route is first read into a `Spec` that keeps the field path of
//! everything in it, so the rules check it and name where to fix it, and then
//! expanded.

use std::collections::BTreeMap;

use serde::Serialize;

use crate::config::{
    audited_by_default, Config, FilledTimeouts, MethodConfig, RateLimit, Required, Resource, Route,
    Service, PUBLIC_SCOPE, VERBS,
};

/// One route as the gateway matches it.
#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct ExpandedRoute {
    pub path: String,
    pub method: String,
    pub scope: String,
    pub upstream: String,
    /// `null` outside a kind scope.
    pub permissions: Option<Required>,
    pub rate_limit: Vec<String>,
    pub audit: bool,
    pub allow_deleted: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub resource: Option<Resource>,
    /// The field path of the route's path, for errors.
    #[serde(skip)]
    pub at: String,
}

/// A service as the gateway is served it: its own fields with `defaults`
/// filled in, and its expanded routes sorted by path, then method.
#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct ExpandedService {
    pub url: String,
    pub timeouts: FilledTimeouts,
    pub max_body_bytes: u64,
    pub routes: Vec<ExpandedRoute>,
}

/// A value and the field path it's written at.
#[derive(Clone, Debug, PartialEq)]
pub struct At<T> {
    pub value: T,
    pub at: String,
}

fn at<T>(value: T, at: impl Into<String>) -> At<T> {
    At {
        value,
        at: at.into(),
    }
}

/// One route of a service file, before expansion.
#[derive(Clone, Debug)]
pub struct Spec {
    pub service: String,
    pub scope: String,
    /// As written: relative to the node in a kind scope.
    pub path: String,
    pub path_at: String,
    pub upstream: Option<At<String>>,
    pub prefix: Option<At<String>>,
    pub verbs: Vec<VerbSpec>,
    pub allow_deleted: Option<At<bool>>,
    /// Route-level rate_limit / resource kept so shadowed declarations are validated too.
    pub route_rate_limit: Option<At<RateLimit>>,
    pub route_resource: Option<At<Resource>>,
}

#[derive(Clone, Debug)]
pub struct VerbSpec {
    pub method: String,
    /// Where a missing `permissions` is reported.
    pub missing_at: String,
    pub permissions: Option<At<Required>>,
    pub rate_limit: Option<At<RateLimit>>,
    pub audit: Option<At<bool>>,
    pub resource: Option<At<Resource>>,
}

pub fn verb_order(method: &str) -> usize {
    VERBS
        .iter()
        .position(|v| *v == method)
        .unwrap_or(VERBS.len())
}

/// Every route of every service, read into specs.
pub fn specs(config: &Config) -> Vec<Spec> {
    config
        .services
        .iter()
        .flat_map(|(name, service)| route_specs(name, service))
        .collect()
}

fn route_specs(name: &str, service: &Service) -> Vec<Spec> {
    let mut out = Vec::new();
    for (scope_name, scope) in &service.scopes {
        let scope_at = format!("services.{name}.scopes.{scope_name}");
        let prefix = scope
            .upstream_prefix
            .clone()
            .map(|p| at(p, format!("{scope_at}.upstream_prefix")));
        for (i, route) in scope.routes.iter().enumerate() {
            let route_at = format!("{scope_at}.routes[{i}]");
            let mut verbs: Vec<VerbSpec> = match &route.methods {
                crate::config::Methods::List(list) => list
                    .iter()
                    .map(|m| verb_spec(m, &route_at, route, None, &route_at))
                    .collect(),
                crate::config::Methods::Map(map) => map
                    .iter()
                    .map(|(m, cfg)| {
                        let verb_at = format!("{route_at}.methods.{m}");
                        verb_spec(m, &route_at, route, cfg.as_ref(), &verb_at)
                    })
                    .collect(),
            };
            verbs.sort_by_key(|v| verb_order(&v.method));
            out.push(Spec {
                service: name.to_string(),
                scope: scope_name.clone(),
                path: route.path.clone(),
                path_at: format!("{route_at}.path"),
                upstream: route
                    .upstream
                    .clone()
                    .map(|u| at(u, format!("{route_at}.upstream"))),
                prefix: prefix.clone(),
                verbs,
                allow_deleted: route
                    .allow_deleted
                    .map(|a| at(a, format!("{route_at}.allow_deleted"))),
                route_rate_limit: route
                    .rate_limit
                    .clone()
                    .map(|r| at(r, format!("{route_at}.rate_limit"))),
                route_resource: route
                    .resource
                    .clone()
                    .map(|r| at(r, format!("{route_at}.resource"))),
            });
        }
    }
    out
}

fn pick<T: Clone>(
    own: Option<&T>,
    fallback: Option<&T>,
    field: &str,
    verb_at: &str,
    route_at: &str,
) -> Option<At<T>> {
    own.map(|v| at(v.clone(), format!("{verb_at}.{field}")))
        .or_else(|| fallback.map(|v| at(v.clone(), format!("{route_at}.{field}"))))
}

/// One verb of a route: its own fields, else the route's.
fn verb_spec(
    method: &str,
    route_at: &str,
    route: &Route,
    verb: Option<&MethodConfig>,
    verb_at: &str,
) -> VerbSpec {
    VerbSpec {
        method: method.to_string(),
        missing_at: format!("{verb_at}.permissions"),
        permissions: pick(
            verb.and_then(|v| v.permissions.as_ref()),
            route.permissions.as_ref(),
            "permissions",
            verb_at,
            route_at,
        ),
        rate_limit: pick(
            verb.and_then(|v| v.rate_limit.as_ref()),
            route.rate_limit.as_ref(),
            "rate_limit",
            verb_at,
            route_at,
        ),
        audit: pick(
            verb.and_then(|v| v.audit.as_ref()),
            route.audit.as_ref(),
            "audit",
            verb_at,
            route_at,
        ),
        resource: pick(
            verb.and_then(|v| v.resource.as_ref()),
            route.resource.as_ref(),
            "resource",
            verb_at,
            route_at,
        ),
    }
}

/// The `{params}` of a path, in order.
pub fn params_of(path: &str) -> Vec<String> {
    let mut out = Vec::new();
    let mut rest = path;
    while let Some(start) = rest.find('{') {
        let after = &rest[start + 1..];
        let Some(end) = after.find('}') else {
            break;
        };
        out.push(after[..end].to_string());
        rest = &after[end + 1..];
    }
    out
}

/// The public path a spec is matched at: in a kind scope, the node's path and
/// then the route's (`/` adds nothing).
pub fn public_path(config: &Config, spec: &Spec) -> String {
    if !config.kinds.contains_key(&spec.scope) {
        return spec.path.clone();
    }
    let node = config.node_path(&spec.scope);
    if spec.path == "/" {
        node
    } else {
        format!("{node}{}", spec.path)
    }
}

/// The spec's routes, one per verb.
pub fn expand_spec(config: &Config, spec: &Spec) -> Vec<ExpandedRoute> {
    let kind_scope = config.kinds.contains_key(&spec.scope);
    let path = public_path(config, spec);
    let default_upstream = || {
        let prefix = spec.prefix.as_ref().map_or("", |p| p.value.as_str());
        match (prefix, spec.path.as_str()) {
            ("", p) => p.to_string(),
            (prefix, "/") => prefix.to_string(),
            (prefix, p) => format!("{prefix}{p}"),
        }
    };
    spec.verbs
        .iter()
        .map(|v| ExpandedRoute {
            path: path.clone(),
            method: v.method.clone(),
            scope: spec.scope.clone(),
            upstream: spec
                .upstream
                .as_ref()
                .map_or_else(default_upstream, |u| u.value.clone()),
            permissions: if kind_scope {
                v.permissions.as_ref().map(|r| r.value.clone())
            } else {
                None
            },
            rate_limit: match v.rate_limit.as_ref().map(|r| &r.value) {
                Some(RateLimit::Policies(p)) => p.clone(),
                Some(RateLimit::Off(_)) => Vec::new(),
                None => config.defaults.rate_limit.clone(),
            },
            audit: spec.scope != PUBLIC_SCOPE
                && v.audit
                    .as_ref()
                    .map_or(audited_by_default(&v.method), |a| a.value),
            allow_deleted: spec.allow_deleted.as_ref().is_some_and(|a| a.value),
            resource: if kind_scope {
                v.resource.as_ref().map(|r| r.value.clone())
            } else {
                None
            },
            at: spec.path_at.clone(),
        })
        .collect()
}

/// Builds the sorted service map from routes already expanded during rule validation.
pub fn assemble_services(
    config: &Config,
    mut routes: BTreeMap<String, Vec<ExpandedRoute>>,
) -> BTreeMap<String, ExpandedService> {
    let defaults = config.defaults.filled();
    let timeouts = defaults.timeouts;
    config
        .services
        .iter()
        .map(|(name, service)| {
            let mut routes = routes.remove(name).unwrap_or_default();
            routes.sort_by(|a, b| {
                a.path
                    .cmp(&b.path)
                    .then_with(|| verb_order(&a.method).cmp(&verb_order(&b.method)))
            });
            let s = ExpandedService {
                url: service.url.clone(),
                timeouts: FilledTimeouts {
                    connect: service
                        .timeouts
                        .connect
                        .clone()
                        .unwrap_or_else(|| timeouts.connect.clone()),
                    read: service
                        .timeouts
                        .read
                        .clone()
                        .unwrap_or_else(|| timeouts.read.clone()),
                },
                max_body_bytes: service.max_body_bytes.unwrap_or(defaults.max_body_bytes),
                routes,
            };
            (name.clone(), s)
        })
        .collect()
}
