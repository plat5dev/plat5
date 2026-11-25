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
    audited_by_default, is_false, Config, FilledTimeouts, Keyword, MethodConfig, RateLimit,
    Required, ResolvedRouteAuthorizer, RouteAuthorizerConfig, Service, PUBLIC_SCOPE, USER_SCOPE,
    VERBS,
};

/// One route as the gateway matches it.
#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct ExpandedRoute {
    pub path: String,
    pub method: String,
    pub scope: String,
    pub upstream: String,
    /// `null` outside a kind scope.
    pub required_permissions: Option<Required>,
    pub rate_limit: Vec<String>,
    pub audit: bool,
    pub allow_deleted: bool,
    #[serde(skip_serializing_if = "is_false")]
    pub allow_public: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub authorizer: Option<ResolvedRouteAuthorizer>,
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
}

#[derive(Clone, Debug)]
pub struct VerbSpec {
    pub method: String,
    /// Where a missing `required_permissions` is reported.
    pub missing_at: String,
    pub required: Option<At<Required>>,
    pub rate_limit: Option<At<RateLimit>>,
    pub audit: Option<At<bool>>,
    pub allow_public: Option<At<bool>>,
    pub authorizer: Option<At<ResolvedRouteAuthorizer>>,
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
        .flat_map(|(name, service)| route_specs(config, name, service))
        .collect()
}

fn route_specs(config: &Config, name: &str, service: &Service) -> Vec<Spec> {
    let mut out = Vec::new();
    let service_authorizer = service
        .authorizer
        .as_ref()
        .map(|a| at(a.clone(), format!("services.{name}.authorizer")));
    for (scope_name, scope) in &service.scopes {
        let scope_at = format!("services.{name}.scopes.{scope_name}");
        let prefix = scope
            .upstream_prefix
            .clone()
            .map(|p| at(p, format!("{scope_at}.upstream_prefix")));
        // A service-level authorizer only inherits into kind scopes (not user/public),
        // whereas an explicit authorizer on a user/public scope is kept so rules rejects it.
        let inherited_service_authz = if scope_name == USER_SCOPE || scope_name == PUBLIC_SCOPE {
            None
        } else {
            service_authorizer.clone()
        };
        let scope_authorizer = scope
            .authorizer
            .as_ref()
            .map(|a| at(a.clone(), format!("{scope_at}.authorizer")))
            .or(inherited_service_authz);
        let kind_authorizer = config
            .kinds
            .get(scope_name)
            .and_then(|k| k.authorizer.as_ref())
            .map(|ka| {
                at(
                    ResolvedRouteAuthorizer::from(ka),
                    format!("kinds.{scope_name}.authorizer"),
                )
            });
        for (i, route) in scope.routes.iter().enumerate() {
            let route_at = format!("{scope_at}.routes[{i}]");
            let route_level = MethodConfig {
                required_permissions: route.required_permissions.clone(),
                rate_limit: route.rate_limit.clone(),
                audit: route.audit,
                allow_public: route.allow_public,
                authorizer: route.authorizer.clone(),
            };
            let mut verbs: Vec<VerbSpec> = match &route.methods {
                crate::config::Methods::List(list) => list
                    .iter()
                    .map(|m| {
                        verb_spec(
                            m,
                            &route_at,
                            &route_level,
                            None,
                            &route_at,
                            scope_authorizer.as_ref(),
                            kind_authorizer.as_ref(),
                        )
                    })
                    .collect(),
                crate::config::Methods::Map(map) => map
                    .iter()
                    .map(|(m, cfg)| {
                        let verb_at = format!("{route_at}.methods.{m}");
                        verb_spec(
                            m,
                            &route_at,
                            &route_level,
                            cfg.as_ref(),
                            &verb_at,
                            scope_authorizer.as_ref(),
                            kind_authorizer.as_ref(),
                        )
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
            });
        }
    }
    out
}

/// One verb of a route: its own fields, else the route's.
fn verb_spec(
    method: &str,
    route_at: &str,
    route: &MethodConfig,
    verb: Option<&MethodConfig>,
    verb_at: &str,
    scope_authorizer: Option<&At<RouteAuthorizerConfig>>,
    kind_authorizer: Option<&At<ResolvedRouteAuthorizer>>,
) -> VerbSpec {
    let field = |f: &str, own: bool| format!("{}.{f}", if own { verb_at } else { route_at });
    let own = verb.cloned().unwrap_or_default();
    let required = own
        .required_permissions
        .map(|r| at(r, field("required_permissions", true)))
        .or_else(|| {
            route
                .required_permissions
                .clone()
                .map(|r| at(r, field("required_permissions", false)))
        });
    let explicit_authorizer = own
        .authorizer
        .map(|a| at(a, field("authorizer", true)))
        .or_else(|| {
            route
                .authorizer
                .clone()
                .map(|a| at(a, field("authorizer", false)))
        })
        .or_else(|| scope_authorizer.cloned());
    // Precedence: verb -> route -> scope -> service -> kind (with `false` opting out).
    // Kind authorizers have no action override, so they inherit only onto routes with
    // explicit permissions (not `any_member`, such as child-list or `/membership` routes).
    let is_any_member = matches!(
        required.as_ref().map(|r| &r.value),
        Some(Required::Keyword(Keyword::AnyMember))
    );
    let authorizer = match explicit_authorizer {
        Some(raw) => raw.value.resolve().map(|r| at(r, raw.at)),
        None if !is_any_member => kind_authorizer.cloned(),
        None => None,
    };
    VerbSpec {
        method: method.to_string(),
        missing_at: format!("{verb_at}.required_permissions"),
        required,
        rate_limit: own
            .rate_limit
            .map(|r| at(r, field("rate_limit", true)))
            .or_else(|| {
                route
                    .rate_limit
                    .clone()
                    .map(|r| at(r, field("rate_limit", false)))
            }),
        audit: own
            .audit
            .map(|a| at(a, field("audit", true)))
            .or_else(|| route.audit.map(|a| at(a, field("audit", false)))),
        allow_public: own
            .allow_public
            .map(|a| at(a, field("allow_public", true)))
            .or_else(|| {
                route
                    .allow_public
                    .map(|a| at(a, field("allow_public", false)))
            }),
        authorizer,
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
            required_permissions: if kind_scope {
                v.required.as_ref().map(|r| r.value.clone())
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
            allow_public: v.allow_public.as_ref().is_some_and(|a| a.value),
            authorizer: if kind_scope {
                v.authorizer.as_ref().map(|a| a.value.clone())
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
