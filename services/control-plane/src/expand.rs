//! Every service's routes as the gateway matches them: one per method, with
//! the full public path, the upstream, and every default filled in
//! (docs/reference/api/internal.md#control-plane-feeds). Identity's and audit's
//! come from their resources' defaults and the deployment's overrides
//! (docs/reference/config/resources.md).
//!
//! Each route is first read into a `Spec` that keeps the field path of
//! everything in it, so the rules check it and name where to fix it, and then
//! expanded.

use std::collections::BTreeMap;

use serde::Serialize;

use crate::config::{
    audited_by_default, Config, FilledTimeouts, MethodConfig, RateLimit, Required, Resource,
    ResourceOverride, Service, VerbOverride, AUDIT, DEFAULT_POLICY, IDENTITY, PUBLIC_SCOPE, VERBS,
};
use crate::resources::{self, ResourceDefault};
use crate::schema::FieldError;

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

/// One route of a service file, or one resource of identity or audit, before
/// expansion.
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
    /// A resource's params, which an overridden path keeps.
    pub keeps: Option<Vec<String>>,
}

#[derive(Clone, Debug)]
pub struct VerbSpec {
    pub method: String,
    /// Where a missing `required_permissions` is reported.
    pub missing_at: String,
    pub required: Option<At<Required>>,
    pub rate_limit: Option<At<RateLimit>>,
    pub audit: Option<At<bool>>,
    /// A resource's upstream for this verb, fixed by Plat5.
    pub upstream: Option<String>,
}

pub fn verb_order(method: &str) -> usize {
    VERBS
        .iter()
        .position(|v| *v == method)
        .unwrap_or(VERBS.len())
}

/// Every route of every service, read into specs. Errors in identity's and
/// audit's overrides that leave no route to check (a kind that doesn't exist,
/// a verb a resource doesn't have) go to `errs`.
pub fn specs(config: &Config, errs: &mut Vec<FieldError>) -> Vec<Spec> {
    let mut out = Vec::new();
    for (name, service) in &config.services {
        if name == IDENTITY || name == AUDIT {
            out.extend(resource_specs(config, name, service, errs));
        } else {
            out.extend(route_specs(name, service));
        }
    }
    out
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
            let route_level = MethodConfig {
                required_permissions: route.required_permissions.clone(),
                rate_limit: route.rate_limit.clone(),
                audit: route.audit,
            };
            let mut verbs: Vec<VerbSpec> = match &route.methods {
                crate::config::Methods::List(list) => list
                    .iter()
                    .map(|m| verb_spec(m, &route_at, &route_level, None, &route_at))
                    .collect(),
                crate::config::Methods::Map(map) => map
                    .iter()
                    .map(|(m, cfg)| {
                        let verb_at = format!("{route_at}.methods.{m}");
                        verb_spec(m, &route_at, &route_level, cfg.as_ref(), &verb_at)
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
                keeps: None,
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
) -> VerbSpec {
    let field = |f: &str, own: bool| format!("{}.{f}", if own { verb_at } else { route_at });
    let own = verb.cloned().unwrap_or_default();
    VerbSpec {
        method: method.to_string(),
        missing_at: format!("{verb_at}.required_permissions"),
        required: own
            .required_permissions
            .map(|r| at(r, field("required_permissions", true)))
            .or_else(|| {
                route
                    .required_permissions
                    .clone()
                    .map(|r| at(r, field("required_permissions", false)))
            }),
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
        upstream: None,
    }
}

/// Identity's or audit's resources: each default, with `resources` and then
/// `kinds.<kind>.resources` laid over it.
fn resource_specs(
    config: &Config,
    name: &str,
    service: &Service,
    errs: &mut Vec<FieldError>,
) -> Vec<Spec> {
    let base = format!("services.{name}");
    let defaults = resources::defaults(config, name);
    for (k, overrides) in &service.kinds {
        let Some(kind) = config.kinds.get(k) else {
            errs.push(FieldError::new(
                format!("{base}.kinds.{k}"),
                format!("{k:?} is not a kind"),
            ));
            continue;
        };
        for r in overrides.resources.keys() {
            let known = defaults
                .iter()
                .any(|d| d.name == r && d.kind.as_deref() == Some(k.as_str()));
            if !known {
                let why = if kind.is_root() {
                    "it's a root kind"
                } else {
                    "it isn't a root kind"
                };
                errs.push(FieldError::new(
                    format!("{base}.kinds.{k}.resources.{r}"),
                    format!("{k} has no {r} resource: {why}"),
                ));
            }
        }
    }

    let mut out = Vec::new();
    for d in &defaults {
        let mut layers: Vec<(String, &Resource, bool)> = Vec::new();
        if d.shared {
            push_layer(
                &mut layers,
                &service.resources,
                d.name,
                format!("{base}.resources"),
                true,
            );
        }
        if let Some(k) = &d.kind {
            if let Some(o) = service.kinds.get(k) {
                push_layer(
                    &mut layers,
                    &o.resources,
                    d.name,
                    format!("{base}.kinds.{k}.resources"),
                    false,
                );
            }
        }
        let spec_at = match (layers.last(), &d.kind) {
            (Some((a, _, _)), _) => a.clone(),
            (None, Some(k)) => format!("{base}.kinds.{k}.resources.{}", d.name),
            (None, None) => format!("{base}.resources.{}", d.name),
        };
        if let Some(spec) = resource_spec(name, d, &defaults, &layers, spec_at, errs) {
            out.push(spec);
        }
    }
    out
}

/// A resource whose collection was turned back on at a layer: present, and
/// otherwise unchanged.
static PRESENT: Resource = Resource::Override(ResourceOverride {
    path: None,
    methods: BTreeMap::new(),
    required_permissions: None,
    rate_limit: None,
    audit: None,
});

/// One layer's override of `name`: its own entry, else what its collections say
/// there. A collection's `false` removes its items too, and turning it back on
/// brings them back; an item's own entry at the same layer wins.
fn push_layer<'a>(
    layers: &mut Vec<(String, &'a Resource, bool)>,
    overrides: &'a BTreeMap<String, Resource>,
    name: &str,
    at: String,
    shared: bool,
) {
    if let Some(r) = overrides.get(name) {
        layers.push((format!("{at}.{name}"), r, shared));
        return;
    }
    for c in resources::collections_of(name) {
        match overrides.get(*c) {
            Some(off @ Resource::Off(_)) => {
                layers.push((format!("{at}.{c}"), off, shared));
                return;
            }
            Some(Resource::Override(_)) => layers.push((format!("{at}.{c}"), &PRESENT, shared)),
            None => {}
        }
    }
}

fn resource_spec(
    service: &str,
    d: &ResourceDefault,
    defaults: &[ResourceDefault],
    layers: &[(String, &Resource, bool)],
    spec_at: String,
    errs: &mut Vec<FieldError>,
) -> Option<Spec> {
    let mut path = at(d.path.clone(), format!("{spec_at}.path"));
    let mut removed = false;
    let mut verbs: Vec<(VerbSpec, bool)> = d
        .verbs
        .iter()
        .map(|v| {
            (
                VerbSpec {
                    method: v.method.to_string(),
                    missing_at: format!("{spec_at}.methods.{}.required_permissions", v.method),
                    required: v.required.clone().map(|r| at(r, spec_at.clone())),
                    rate_limit: None,
                    audit: None,
                    upstream: Some(v.upstream.clone()),
                },
                true,
            )
        })
        .collect();

    for (layer_at, resource, shared) in layers {
        let o = match resource {
            Resource::Off(_) => {
                removed = true;
                continue;
            }
            Resource::Override(o) => o,
        };
        removed = false;
        if let Some(p) = &o.path {
            path = at(p.clone(), format!("{layer_at}.path"));
        }
        for (v, _) in verbs.iter_mut() {
            set_fields(
                v,
                &MethodConfig {
                    required_permissions: o.required_permissions.clone(),
                    rate_limit: o.rate_limit.clone(),
                    audit: o.audit,
                },
                layer_at,
            );
        }
        for (method, over) in &o.methods {
            let Some((v, present)) = verbs.iter_mut().find(|(v, _)| &v.method == method) else {
                // A shared override names a verb the resource has at another
                // kind (membership's DELETE, on roots only).
                let elsewhere = *shared
                    && defaults
                        .iter()
                        .filter(|o| o.name == d.name)
                        .any(|o| o.verbs.iter().any(|v| v.method == method));
                if !elsewhere {
                    errs.push(FieldError::new(
                        format!("{layer_at}.methods.{method}"),
                        format!("{} has no {method} here", d.name),
                    ));
                }
                continue;
            };
            match over {
                Some(VerbOverride::Off(_)) => *present = false,
                None => *present = true,
                Some(VerbOverride::Set(cfg)) => {
                    *present = true;
                    set_fields(v, cfg, &format!("{layer_at}.methods.{method}"));
                }
            }
        }
    }
    if removed {
        return None;
    }

    let verbs: Vec<VerbSpec> = verbs
        .into_iter()
        .filter(|(_, present)| *present)
        .map(|(mut v, _)| {
            if let (Some(kind), Some(r)) = (&d.kind, v.required.as_mut()) {
                if let Required::Permissions(perms) = &mut r.value {
                    for p in perms.iter_mut() {
                        *p = p.replace("{kind}", kind);
                    }
                }
            }
            v
        })
        .collect();
    if verbs.is_empty() {
        return None;
    }
    Some(Spec {
        service: service.to_string(),
        scope: d.scope.clone(),
        keeps: Some(params_of(&d.path)),
        path: path.value,
        path_at: path.at,
        upstream: None,
        prefix: None,
        verbs,
        allow_deleted: d.allow_deleted.then(|| at(true, spec_at)),
    })
}

/// Lays one override's fields over a verb, each at `{base}.{field}`.
fn set_fields(v: &mut VerbSpec, cfg: &MethodConfig, base: &str) {
    if let Some(r) = &cfg.required_permissions {
        v.required = Some(at(r.clone(), format!("{base}.required_permissions")));
    }
    if let Some(r) = &cfg.rate_limit {
        v.rate_limit = Some(at(r.clone(), format!("{base}.rate_limit")));
    }
    if let Some(a) = cfg.audit {
        v.audit = Some(at(a, format!("{base}.audit")));
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
            upstream: v
                .upstream
                .clone()
                .or_else(|| spec.upstream.as_ref().map(|u| u.value.clone()))
                .unwrap_or_else(default_upstream),
            required_permissions: if kind_scope {
                v.required.as_ref().map(|r| r.value.clone())
            } else {
                None
            },
            rate_limit: match v.rate_limit.as_ref().map(|r| &r.value) {
                Some(RateLimit::Policies(p)) => p.clone(),
                Some(RateLimit::Off(_)) => Vec::new(),
                None => vec![DEFAULT_POLICY.to_string()],
            },
            audit: spec.scope != PUBLIC_SCOPE
                && v.audit
                    .as_ref()
                    .map_or(audited_by_default(&v.method), |a| a.value),
            allow_deleted: spec.allow_deleted.as_ref().is_some_and(|a| a.value),
            at: spec.path_at.clone(),
        })
        .collect()
}

/// Every service, expanded. The config passed the rules.
pub fn expand(config: &Config) -> BTreeMap<String, ExpandedService> {
    let mut errs = Vec::new();
    let mut routes: BTreeMap<String, Vec<ExpandedRoute>> = BTreeMap::new();
    for spec in specs(config, &mut errs) {
        routes
            .entry(spec.service.clone())
            .or_default()
            .extend(expand_spec(config, &spec));
    }
    let (timeouts, max_body_bytes) = config.defaults.filled();
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
                max_body_bytes: service.max_body_bytes.unwrap_or(max_body_bytes),
                routes,
            };
            (name.clone(), s)
        })
        .collect()
}
