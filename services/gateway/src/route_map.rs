//! The routes the gateway serves, built from the control plane's stored form
//! (docs/reference/config/routes.md): full paths, list-form `methods`, each
//! route with its scope. A snapshot the gateway can't build is refused whole.

use std::collections::{BTreeMap, HashMap, HashSet};
use std::time::Duration;

use regex::Regex;
use serde::Deserialize;

use crate::kinds::Kinds;

/// A service in the gateway feed, in the stored form. The schema has checked
/// the shape; these are the fields the gateway reads.
#[derive(Clone, Debug, Deserialize)]
pub struct ServiceDoc {
    pub url: String,
    #[serde(default)]
    pub rate_limits: BTreeMap<String, PolicyDoc>,
    #[serde(default)]
    pub timeouts: Option<TimeoutsDoc>,
    #[serde(default)]
    pub route_prefix: Option<String>,
    pub routes: Vec<RouteDoc>,
}

#[derive(Clone, Debug, Deserialize)]
pub struct TimeoutsDoc {
    pub connect_ms: Option<u64>,
    pub read_ms: Option<u64>,
}

#[derive(Clone, Debug, Deserialize)]
pub struct PolicyDoc {
    pub requests: u64,
    pub window_seconds: u64,
    #[serde(default)]
    pub count_at: Option<String>,
    #[serde(default)]
    pub shared: bool,
}

#[derive(Clone, Debug, Deserialize)]
pub struct RouteDoc {
    pub path: String,
    pub scope: String,
    #[serde(default)]
    pub upstream: Option<String>,
    /// The stored form is always a list; a methods map is refused at decode.
    pub methods: Vec<String>,
    #[serde(default)]
    pub required_labels: Option<Vec<String>>,
    #[serde(default)]
    pub rate_limit: Option<RateLimitDoc>,
    #[serde(default)]
    pub audit: Option<bool>,
    #[serde(default)]
    pub resolve_deleted: bool,
}

#[derive(Clone, Debug, Deserialize)]
#[serde(untagged)]
pub enum RateLimitDoc {
    Off(bool),
    Name(String),
    Names(Vec<String>),
    Inline { requests: u64, window_seconds: u64 },
}

/// What a route addresses.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Scope {
    /// No subject, no admit.
    Public,
    /// The sign-in kind: an empty chain, the person's directory member.
    SignIn(String),
    /// A kind: a node of it, through its chain of ancestors.
    Kind(String),
}

impl Scope {
    /// The scope's name as a route writes it, and as admit takes it.
    pub fn name(&self) -> &str {
        match self {
            Scope::Public => "public",
            Scope::SignIn(k) | Scope::Kind(k) => k,
        }
    }
}

#[derive(Clone, Debug)]
pub struct Route {
    /// The service name in the config.
    pub service: String,
    pub base_url: String,
    pub path: String,
    pub methods: HashSet<String>,
    /// Absolute upstream template. None means proxy `path` unchanged.
    pub upstream: Option<String>,
    pub scope: Scope,
    /// The kinds in the path, root first; each is also its param's name. Empty on
    /// `public` and sign-in routes.
    pub chain: Vec<String>,
    /// Any one admits. Empty: any member of the tree, or no check at all.
    pub required_labels: Vec<String>,
    pub limiter: RouteLimiter,
    /// The route's `audit` override. None = method default.
    pub audit: Option<bool>,
    pub resolve_deleted: bool,
    /// The service's `timeouts`. None = the gateway's `UPSTREAM_*_TIMEOUT_MS`.
    pub timeouts: UpstreamTimeouts,
}

impl Route {
    /// Whether a request with `method` goes in an audit log (docs/concepts/audit.md#which-log).
    /// `public` has no log. Writes are audited by default; the route's `audit` overrides.
    pub fn audited(&self, method: &str) -> bool {
        if self.scope == Scope::Public {
            return false;
        }
        self.audit
            .unwrap_or(matches!(method, "POST" | "PUT" | "PATCH" | "DELETE"))
    }

    /// Whether `param` is one of the chain's: its value is a slug or id, never sent on.
    pub fn is_chain_param(&self, param: &str) -> bool {
        self.chain.iter().any(|k| k == param)
    }
}

/// A service's upstream timeouts. Each one left out uses the gateway default.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct UpstreamTimeouts {
    pub connect: Option<Duration>,
    pub read: Option<Duration>,
}

/// Resolved at load from `rate_limit` and the service's `rate_limits`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum RouteLimiter {
    /// The gateway fallback (`RATE_LIMIT_*`), counted at the route's own node.
    Inherit,
    Unlimited,
    /// Every one must pass.
    Policies(Vec<Policy>),
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Policy {
    pub requests: u64,
    pub window_seconds: u64,
    pub bucket: LimitBucket,
    /// The kind whose node holds the bucket. None = the route's own.
    pub count_at: Option<String>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum LimitBucket {
    /// `{METHOD} {path} {node_id}`
    MethodPath,
    /// `{prefix}:{node_id}`: `service:name`, or a shared `name`.
    Named(String),
}

struct CompiledRoute {
    regex: Regex,
    route: Route,
    /// Static (non-param) segment count — higher is more specific.
    static_segments: usize,
    /// Path parameter count — lower is more specific when static equal.
    param_count: usize,
}

#[derive(Default)]
pub struct RouteMap {
    routes: Vec<CompiledRoute>,
}

impl RouteMap {
    /// Build from the feed's services. Err names the first thing that can't be
    /// built: the caller refuses the whole snapshot.
    ///
    /// Routes are sorted by specificity (more static segments, fewer params,
    /// longer path, path string, service) so first-match is deterministic.
    pub fn from_feed(
        services: &BTreeMap<String, ServiceDoc>,
        kinds: &Kinds,
    ) -> Result<Self, String> {
        let mut routes = Vec::new();
        for (name, service) in services {
            if service.route_prefix.is_some() {
                return Err(format!(
                    "services.{name}: route_prefix in the stored form (it is expanded at apply)"
                ));
            }
            let timeouts = UpstreamTimeouts {
                connect: service
                    .timeouts
                    .as_ref()
                    .and_then(|t| t.connect_ms)
                    .map(Duration::from_millis),
                read: service
                    .timeouts
                    .as_ref()
                    .and_then(|t| t.read_ms)
                    .map(Duration::from_millis),
            };
            for (i, doc) in service.routes.iter().enumerate() {
                let route = build_route(name, service, doc, kinds, timeouts)
                    .map_err(|e| format!("services.{name}.routes[{i}] ({}): {e}", doc.path))?;
                let (static_segments, param_count) = path_specificity(&route.path);
                routes.push(CompiledRoute {
                    regex: path_to_regex(&route.path)
                        .map_err(|e| format!("services.{name}.routes[{i}]: {e}"))?,
                    route,
                    static_segments,
                    param_count,
                });
            }
        }
        routes.sort_by(|a, b| {
            b.static_segments
                .cmp(&a.static_segments)
                .then_with(|| a.param_count.cmp(&b.param_count))
                .then_with(|| b.route.path.len().cmp(&a.route.path.len()))
                .then_with(|| a.route.path.cmp(&b.route.path))
                .then_with(|| a.route.service.cmp(&b.route.service))
        });
        Ok(Self { routes })
    }

    pub fn find_route(
        &self,
        external_path: &str,
        method: &str,
    ) -> Option<(&Route, HashMap<String, String>)> {
        for compiled in &self.routes {
            if !compiled.route.methods.contains(method) {
                continue;
            }
            if let Some(caps) = compiled.regex.captures(external_path) {
                let params = compiled
                    .regex
                    .capture_names()
                    .flatten()
                    .filter_map(|key| {
                        caps.name(key)
                            .map(|v| (key.to_string(), v.as_str().to_string()))
                    })
                    .collect();
                return Some((&compiled.route, params));
            }
        }
        None
    }

    pub fn route_count(&self) -> usize {
        self.routes.len()
    }

    /// Every route, in match order.
    pub fn routes(&self) -> impl Iterator<Item = &Route> {
        self.routes.iter().map(|c| &c.route)
    }
}

fn build_route(
    service_name: &str,
    service: &ServiceDoc,
    doc: &RouteDoc,
    kinds: &Kinds,
    timeouts: UpstreamTimeouts,
) -> Result<Route, String> {
    let scope = if doc.scope == "public" {
        Scope::Public
    } else if doc.scope == kinds.sign_in() {
        Scope::SignIn(doc.scope.clone())
    } else if kinds.contains(&doc.scope) {
        Scope::Kind(doc.scope.clone())
    } else {
        return Err(format!("scope {} is not a kind", doc.scope));
    };

    let params = path_params(&doc.path)?;
    let chain: Vec<String> = params
        .iter()
        .filter(|p| kinds.contains(p))
        .cloned()
        .collect();
    match &scope {
        Scope::Kind(kind) => {
            if chain.last() != Some(kind) || !kinds.is_chain(&chain) {
                return Err(format!(
                    "path params [{}] are not a chain down to {kind}",
                    chain.join(", ")
                ));
            }
        }
        _ => {
            if !chain.is_empty() {
                return Err(format!("chain params on a {} route", scope.name()));
            }
        }
    }

    let required_labels = doc.required_labels.clone().unwrap_or_default();
    match &scope {
        Scope::Kind(_) if doc.required_labels.is_none() => {
            return Err("required_labels missing".to_string());
        }
        Scope::Public | Scope::SignIn(_) if !required_labels.is_empty() => {
            return Err(format!("required_labels on a {} route", scope.name()));
        }
        _ => {}
    }
    if doc.resolve_deleted && !matches!(scope, Scope::Kind(_)) {
        return Err(format!("resolve_deleted on a {} route", scope.name()));
    }

    match &doc.upstream {
        Some(template) => check_upstream(template, &scope, &chain, &params)?,
        None if !chain.is_empty() => {
            return Err("no upstream on a path with chain params".to_string());
        }
        None => {}
    }

    let limiter = bind_limiter(service_name, service, doc, &scope, &chain, kinds)?;

    Ok(Route {
        service: service_name.to_string(),
        base_url: service.url.clone(),
        path: doc.path.clone(),
        methods: doc.methods.iter().cloned().collect(),
        upstream: doc.upstream.clone(),
        scope,
        chain,
        required_labels,
        limiter,
        audit: doc.audit,
        resolve_deleted: doc.resolve_deleted,
        timeouts,
    })
}

/// Every `{subject.*}` names a field the scope provides; every `{path.*}` a
/// non-chain param of the path.
fn check_upstream(
    template: &str,
    scope: &Scope,
    chain: &[String],
    params: &[String],
) -> Result<(), String> {
    for placeholder in placeholders(template)? {
        let ok = match placeholder.split_once('.') {
            Some(("subject", "member_id")) => *scope != Scope::Public,
            Some(("subject", field)) => field
                .strip_suffix("_id")
                .is_some_and(|kind| chain.iter().any(|k| k == kind)),
            Some(("path", name)) => {
                params.iter().any(|p| p == name) && !chain.iter().any(|k| k == name)
            }
            _ => false,
        };
        if !ok {
            return Err(format!("upstream placeholder {{{placeholder}}}"));
        }
    }
    Ok(())
}

fn placeholders(template: &str) -> Result<Vec<&str>, String> {
    let mut out = Vec::new();
    let mut rest = template;
    while let Some(start) = rest.find('{') {
        let after = &rest[start + 1..];
        let end = after
            .find('}')
            .ok_or_else(|| "unclosed { in upstream".to_string())?;
        out.push(&after[..end]);
        rest = &after[end + 1..];
    }
    Ok(out)
}

fn bind_limiter(
    service_name: &str,
    service: &ServiceDoc,
    doc: &RouteDoc,
    scope: &Scope,
    chain: &[String],
    kinds: &Kinds,
) -> Result<RouteLimiter, String> {
    let names: Vec<&String> = match &doc.rate_limit {
        None => return Ok(RouteLimiter::Inherit),
        Some(RateLimitDoc::Off(false)) => return Ok(RouteLimiter::Unlimited),
        Some(RateLimitDoc::Off(true)) => return Err("rate_limit: true".to_string()),
        Some(RateLimitDoc::Inline {
            requests,
            window_seconds,
        }) => {
            return Ok(RouteLimiter::Policies(vec![Policy {
                requests: *requests,
                window_seconds: *window_seconds,
                bucket: LimitBucket::MethodPath,
                count_at: None,
            }]))
        }
        Some(RateLimitDoc::Name(name)) => vec![name],
        Some(RateLimitDoc::Names(names)) => names.iter().collect(),
    };
    let mut policies = Vec::with_capacity(names.len());
    for name in names {
        let policy = service
            .rate_limits
            .get(name)
            .ok_or_else(|| format!("rate_limit {name} is not defined on this service"))?;
        if let Some(count_at) = &policy.count_at {
            let ok = match scope {
                Scope::Public => false,
                Scope::SignIn(kind) => count_at == kind,
                Scope::Kind(_) => chain.iter().any(|k| k == count_at),
            };
            if !ok || !kinds.contains(count_at) {
                return Err(format!(
                    "rate_limit {name} counts at {count_at}, not in this route's chain"
                ));
            }
        }
        policies.push(Policy {
            requests: policy.requests,
            window_seconds: policy.window_seconds,
            bucket: LimitBucket::Named(if policy.shared {
                name.clone()
            } else {
                format!("{service_name}:{name}")
            }),
            count_at: policy.count_at.clone(),
        });
    }
    Ok(RouteLimiter::Policies(policies))
}

/// The `{param}` names in a path, in order.
fn path_params(path: &str) -> Result<Vec<String>, String> {
    let mut out = Vec::new();
    for segment in path.split('/') {
        if let Some(name) = segment.strip_prefix('{').and_then(|s| s.strip_suffix('}')) {
            if name.is_empty() {
                return Err("empty path parameter name".to_string());
            }
            if out.iter().any(|p| p == name) {
                return Err(format!("path param {{{name}}} twice"));
            }
            out.push(name.to_string());
        } else if segment.contains('{') || segment.contains('}') {
            return Err(format!("path segment {segment} mixes text and a param"));
        }
    }
    Ok(out)
}

/// Static segment count and path-param count for specificity ranking.
fn path_specificity(path: &str) -> (usize, usize) {
    let mut static_segments = 0usize;
    let mut param_count = 0usize;
    for segment in path.split('/').filter(|s| !s.is_empty()) {
        if segment.starts_with('{') && segment.ends_with('}') {
            param_count += 1;
        } else {
            static_segments += 1;
        }
    }
    (static_segments, param_count)
}

/// Convert a route path like `/api/widgets/{id}` into a regex that
/// matches the literal segments and captures path parameters.
fn path_to_regex(path: &str) -> Result<Regex, regex::Error> {
    let mut pattern = String::from("^");
    for (i, segment) in path.split('/').enumerate() {
        if i > 0 {
            pattern.push('/');
        }
        match segment.strip_prefix('{').and_then(|s| s.strip_suffix('}')) {
            Some(name) => pattern.push_str(&format!("(?P<{}>[^/]+)", regex::escape(name))),
            None => pattern.push_str(&regex::escape(segment)),
        }
    }
    pattern.push('$');
    Regex::new(&pattern)
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn kinds() -> Kinds {
        let docs = serde_json::from_value(json!({
            "directory": { "sign_in": "idp" },
            "organization": {},
            "project": { "parent": "organization" }
        }))
        .unwrap();
        Kinds::from_feed(&docs).unwrap()
    }

    fn build(services: serde_json::Value) -> Result<RouteMap, String> {
        let services: BTreeMap<String, ServiceDoc> = serde_json::from_value(services).unwrap();
        RouteMap::from_feed(&services, &kinds())
    }

    fn one_route(route: serde_json::Value) -> Result<RouteMap, String> {
        build(json!({ "svc": { "url": "http://svc:3000", "routes": [route] } }))
    }

    #[test]
    fn specificity_prefers_static_over_param() {
        let map = build(json!({
            "b": { "url": "http://b:1", "routes": [{ "path": "/api/{id}", "scope": "public", "methods": ["GET"] }] },
            "a": { "url": "http://a:1", "routes": [{ "path": "/api/health", "scope": "public", "methods": ["GET"] }] }
        }))
        .unwrap();
        let (r, _) = map.find_route("/api/health", "GET").unwrap();
        assert_eq!(r.service, "a");
        let (r, params) = map.find_route("/api/x", "GET").unwrap();
        assert_eq!(r.service, "b");
        assert_eq!(params["id"], "x");
    }

    #[test]
    fn a_kind_route_knows_its_chain() {
        let map = one_route(json!({
            "path": "/orgs/{organization}/projects/{project}/plans/{key}",
            "scope": "project",
            "upstream": "/projects/{subject.project_id}/plans/{path.key}",
            "methods": ["GET"],
            "required_labels": ["project:read"]
        }))
        .unwrap();
        let (r, params) = map
            .find_route("/orgs/acme/projects/web/plans/pro", "GET")
            .unwrap();
        assert_eq!(r.scope, Scope::Kind("project".into()));
        assert_eq!(r.chain, vec!["organization", "project"]);
        assert!(r.is_chain_param("organization") && !r.is_chain_param("key"));
        assert_eq!(params["key"], "pro");
        assert!(map
            .find_route("/orgs/acme/projects/web/plans/pro", "POST")
            .is_none());
    }

    #[test]
    fn service_timeouts_reach_its_routes() {
        let map = build(json!({
            "slow": { "url": "http://s:1", "timeouts": { "read_ms": 120000 },
                      "routes": [{ "path": "/slow", "scope": "public", "methods": ["GET"] }] }
        }))
        .unwrap();
        let (slow, _) = map.find_route("/slow", "GET").unwrap();
        assert_eq!(slow.timeouts.read, Some(Duration::from_secs(120)));
        assert_eq!(slow.timeouts.connect, None);
    }

    #[test]
    fn policies_bind_buckets_and_count_at() {
        let map = build(json!({
            "e10s": {
                "url": "http://e10s:3000",
                "rate_limits": {
                    "per-project": { "requests": 100, "window_seconds": 1, "count_at": "project" },
                    "per-org": { "requests": 300, "window_seconds": 1, "count_at": "organization", "shared": true }
                },
                "routes": [{
                    "path": "/orgs/{organization}/projects/{project}/check",
                    "scope": "project",
                    "upstream": "/projects/{subject.project_id}/check",
                    "methods": ["POST"],
                    "required_labels": ["check"],
                    "rate_limit": ["per-project", "per-org"]
                }, {
                    "path": "/orgs/{organization}/projects/{project}/inline",
                    "scope": "project",
                    "upstream": "/projects/{subject.project_id}/inline",
                    "methods": ["POST"],
                    "required_labels": ["check"],
                    "rate_limit": { "requests": 5, "window_seconds": 60 }
                }]
            }
        }))
        .unwrap();
        let (r, _) = map.find_route("/orgs/a/projects/b/check", "POST").unwrap();
        assert_eq!(
            r.limiter,
            RouteLimiter::Policies(vec![
                Policy {
                    requests: 100,
                    window_seconds: 1,
                    bucket: LimitBucket::Named("e10s:per-project".into()),
                    count_at: Some("project".into()),
                },
                Policy {
                    requests: 300,
                    window_seconds: 1,
                    bucket: LimitBucket::Named("per-org".into()),
                    count_at: Some("organization".into()),
                },
            ])
        );
        let (r, _) = map.find_route("/orgs/a/projects/b/inline", "POST").unwrap();
        assert!(
            matches!(&r.limiter, RouteLimiter::Policies(p) if p[0].bucket == LimitBucket::MethodPath)
        );
    }

    #[test]
    fn refuses_what_it_cannot_build() {
        let kind_route = |path: &str, upstream: &str| {
            json!({ "path": path, "scope": "project", "upstream": upstream,
                    "methods": ["GET"], "required_labels": [] })
        };
        for (route, why) in [
            (
                json!({ "path": "/x", "scope": "folder", "methods": ["GET"] }),
                "not a kind",
            ),
            (
                kind_route("/projects/{project}", "/p/{subject.project_id}"),
                "not a chain",
            ),
            (
                kind_route("/p/{project}/o/{organization}", "/p/{subject.project_id}"),
                "not a chain",
            ),
            (
                kind_route("/o/{organization}/p/{project}", "/p/{subject.folder_id}"),
                "placeholder",
            ),
            (
                kind_route("/o/{organization}/p/{project}", "/p/{path.project}"),
                "placeholder",
            ),
            (
                kind_route("/o/{organization}/p/{project}", "/p/{project}"),
                "placeholder",
            ),
            (
                json!({ "path": "/o/{organization}/p/{project}", "scope": "project", "methods": ["GET"], "required_labels": [] }),
                "no upstream",
            ),
            (
                json!({ "path": "/o/{organization}/p/{project}", "scope": "project", "upstream": "/p", "methods": ["GET"] }),
                "required_labels missing",
            ),
            (
                json!({ "path": "/me", "scope": "directory", "methods": ["GET"], "required_labels": ["x"] }),
                "required_labels on",
            ),
            (
                json!({ "path": "/me", "scope": "directory", "methods": ["GET"], "resolve_deleted": true }),
                "resolve_deleted",
            ),
            (
                json!({ "path": "/x/{organization}", "scope": "public", "methods": ["GET"] }),
                "chain params",
            ),
            (
                json!({ "path": "/x", "scope": "public", "upstream": "/{subject.member_id}", "methods": ["GET"] }),
                "placeholder",
            ),
            (
                json!({ "path": "/x", "scope": "public", "methods": ["GET"], "rate_limit": "nope" }),
                "not defined",
            ),
        ] {
            let err = one_route(route.clone())
                .err()
                .unwrap_or_else(|| panic!("built {route}"));
            assert!(err.contains(why), "{route}: {err}");
        }

        let count_at_outside = build(json!({ "svc": {
            "url": "http://svc:1",
            "rate_limits": { "p": { "requests": 1, "window_seconds": 1, "count_at": "project" } },
            "routes": [{ "path": "/o/{organization}", "scope": "organization",
                         "upstream": "/o/{subject.organization_id}", "methods": ["GET"],
                         "required_labels": [], "rate_limit": "p" }]
        }}));
        assert!(count_at_outside
            .err()
            .unwrap()
            .contains("not in this route's chain"));

        let prefixed = build(json!({ "svc": {
            "url": "http://svc:1", "route_prefix": "/v1",
            "routes": [{ "path": "/x", "scope": "public", "methods": ["GET"] }]
        }}));
        assert!(prefixed.err().unwrap().contains("route_prefix"));
    }

    #[test]
    fn audit_defaults_by_method_and_never_on_public() {
        let map = build(json!({ "svc": { "url": "http://svc:1", "routes": [
            { "path": "/me", "scope": "directory", "methods": ["GET", "POST"] },
            { "path": "/me/quiet", "scope": "directory", "methods": ["POST"], "audit": false },
            { "path": "/me/loud", "scope": "directory", "methods": ["GET"], "audit": true },
            { "path": "/open", "scope": "public", "methods": ["POST"] }
        ]}}))
        .unwrap();
        let route = |p: &str, m: &str| map.find_route(p, m).unwrap().0.audited(m);
        assert!(route("/me", "POST"));
        assert!(!route("/me", "GET"));
        assert!(!route("/me/quiet", "POST"));
        assert!(route("/me/loud", "GET"));
        assert!(!route("/open", "POST"));
    }
}
