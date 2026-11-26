//! The routes the gateway serves, built from the control plane's gateway feed
//! (docs/reference/api/internal.md#control-plane-feeds): one route per method,
//! with its upstream, permissions, policies and audit already filled in. The
//! control plane validated the config; the gateway parses it and builds its
//! table. A snapshot it can't build is refused whole.

use std::collections::{BTreeMap, HashMap};
use std::sync::Arc;
use std::time::Duration;

use serde::Deserialize;

use crate::authorizer::{AuthorizerSpec, RouteResource, RouteResourceDoc};
use crate::duration::parse_duration;
use crate::kinds::{resolve_host, Kinds, RootHost};

mod upstream;

pub use upstream::{BadParam, Upstream, Values};

/// A service in the gateway feed: its settings with defaults filled in, and its
/// expanded routes.
#[derive(Clone, Debug, Deserialize)]
pub struct ServiceDoc {
    pub url: String,
    pub timeouts: TimeoutsDoc,
    pub max_body_bytes: u64,
    pub routes: Vec<RouteDoc>,
}

#[derive(Clone, Debug, Deserialize)]
pub struct TimeoutsDoc {
    pub connect: String,
    pub read: String,
}

#[derive(Clone, Debug, Deserialize)]
pub struct RouteDoc {
    pub path: String,
    pub method: String,
    pub scope: String,
    pub upstream: String,
    pub permissions: Option<RequiredDoc>,
    pub rate_limit: Vec<String>,
    pub audit: bool,
    pub allow_deleted: bool,
    #[serde(default)]
    pub resource: Option<RouteResourceDoc>,
}

/// A list of permissions, or `"any_member"`.
#[derive(Clone, Debug, Deserialize)]
#[serde(untagged)]
pub enum RequiredDoc {
    Permissions(Vec<String>),
    Word(String),
}

/// Per-tier limit override inside a rate-limit policy.
#[derive(Clone, Debug, Deserialize)]
pub struct TierLimitDoc {
    pub requests: u64,
    pub window: String,
}

/// A rate-limit policy from the feed's `rate_limits`.
#[derive(Clone, Debug, Deserialize)]
pub struct PolicyDoc {
    pub requests: u64,
    pub window: String,
    #[serde(default)]
    pub count_at: Option<String>,
    #[serde(default)]
    pub tiers: BTreeMap<String, TierLimitDoc>,
}

/// What a route addresses.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Scope {
    /// No caller, no admit.
    Public,
    /// The caller as a person: an empty chain, the user id.
    User,
    /// A node of this kind, through its chain of ancestors.
    Kind(String),
}

impl Scope {
    /// The scope's name as a route writes it.
    pub fn name(&self) -> &str {
        match self {
            Scope::Public => "public",
            Scope::User => "user",
            Scope::Kind(kind) => kind,
        }
    }
}

/// What a route requires of the caller's permissions on its node.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Required {
    /// `user` and `public` routes: no permission check.
    Nothing,
    /// Any member of the tree, guests included.
    AnyMember,
    /// Any one of these.
    AnyOf(Vec<String>),
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct TierLimit {
    pub requests: u64,
    pub window_seconds: u64,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Policy {
    pub name: String,
    pub requests: u64,
    pub window_seconds: u64,
    /// The chain position of the `count_at` kind. None = each caller at the route's node.
    pub count_at: Option<usize>,
    /// Optional per-root-tier overrides keyed by the root's `rate_limit_tier`.
    pub tiers: BTreeMap<String, TierLimit>,
}

impl Policy {
    /// Effective `(requests, window_seconds)` for an optional root tier.
    pub fn effective(&self, tier: Option<&str>) -> (u64, u64) {
        if let Some(limit) = tier.and_then(|name| self.tiers.get(name)) {
            (limit.requests, limit.window_seconds)
        } else {
            (self.requests, self.window_seconds)
        }
    }
}

fn parse_tiers(
    policy_name: &str,
    tiers: &BTreeMap<String, TierLimitDoc>,
) -> Result<BTreeMap<String, TierLimit>, String> {
    tiers
        .iter()
        .map(|(tier, doc)| {
            let window_seconds = parse_duration(&doc.window)
                .filter(|d| *d >= Duration::from_secs(1))
                .ok_or_else(|| {
                    format!(
                        "rate_limit {policy_name} tier {tier:?} has invalid window {:?} (must be at least 1s)",
                        doc.window
                    )
                })?
                .as_secs();
            Ok((
                tier.clone(),
                TierLimit {
                    requests: doc.requests,
                    window_seconds,
                },
            ))
        })
        .collect()
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Timeouts {
    pub connect: Duration,
    pub read: Duration,
}

#[derive(Debug)]
pub struct Route {
    /// The service name in the config.
    pub service: String,
    pub base_url: String,
    /// The public path template, as audit and the logs name the route.
    pub path: String,
    pub method: String,
    pub scope: Scope,
    /// The kinds of the route's node chain, root first. Empty on `user` and `public`.
    pub chain: Vec<String>,
    pub upstream: Upstream,
    pub required: Required,
    /// Every one is checked at once.
    pub policies: Vec<Policy>,
    pub audit: bool,
    pub allow_deleted: bool,
    pub resource: Option<RouteResource>,
    pub timeouts: Timeouts,
    pub max_body_bytes: u64,
}

/// A request matched to a route: the chain params' values (slugs or ids, as
/// written, one per chain kind) and the route's own params, as matched.
#[derive(Clone, Debug)]
pub struct Matched {
    pub route: Arc<Route>,
    pub refs: Vec<String>,
    pub params: BTreeMap<String, String>,
}

#[derive(Debug)]
enum Segment {
    Literal(String),
    /// A chain param, by its position in the chain.
    Chain(usize),
    Param(String),
}

struct CompiledRoute {
    segments: Vec<Segment>,
    route: Arc<Route>,
}

#[derive(Default)]
pub struct RouteMap {
    /// Per method, in match order: a literal segment beats a param, from the left.
    by_method: HashMap<String, Vec<CompiledRoute>>,
    /// Host templates on root kinds for subdomain-based root resolution.
    host_routes: Vec<RootHost>,
}

impl RouteMap {
    /// Build from the feed. Err names the first route that can't be built: the
    /// caller refuses the whole snapshot.
    #[cfg(test)]
    pub fn from_feed(
        services: &BTreeMap<String, ServiceDoc>,
        rate_limits: &BTreeMap<String, PolicyDoc>,
        kinds: &Kinds,
    ) -> Result<Self, String> {
        Self::from_feed_with_authorizer(services, rate_limits, kinds, None)
    }

    #[cfg(test)]
    pub fn from_feed_with_authorizer(
        services: &BTreeMap<String, ServiceDoc>,
        rate_limits: &BTreeMap<String, PolicyDoc>,
        kinds: &Kinds,
        authorizer: Option<&AuthorizerSpec>,
    ) -> Result<Self, String> {
        let compiled_policies = compile_policies(rate_limits)?;
        Self::from_compiled(services, &compiled_policies, kinds, authorizer)
    }

    pub fn from_compiled(
        services: &BTreeMap<String, ServiceDoc>,
        compiled_policies: &BTreeMap<String, CompiledPolicy>,
        kinds: &Kinds,
        authorizer: Option<&AuthorizerSpec>,
    ) -> Result<Self, String> {
        let mut by_method: HashMap<String, Vec<CompiledRoute>> = HashMap::new();
        for (name, service) in services {
            let timeouts = Timeouts {
                connect: parse_duration(&service.timeouts.connect)
                    .ok_or_else(|| format!("services.{name}.timeouts.connect"))?,
                read: parse_duration(&service.timeouts.read)
                    .ok_or_else(|| format!("services.{name}.timeouts.read"))?,
            };
            for (i, doc) in service.routes.iter().enumerate() {
                let compiled = build_route(
                    name,
                    service,
                    timeouts,
                    doc,
                    compiled_policies,
                    kinds,
                    authorizer,
                )
                .map_err(|e| {
                    format!(
                        "services.{name}.routes[{i}] ({} {}): {e}",
                        doc.method, doc.path
                    )
                })?;
                by_method
                    .entry(doc.method.clone())
                    .or_default()
                    .push(compiled);
            }
        }
        for routes in by_method.values_mut() {
            routes.sort_by(|a, b| {
                shape(&a.segments)
                    .cmp(&shape(&b.segments))
                    .then_with(|| a.route.path.cmp(&b.route.path))
                    .then_with(|| a.route.service.cmp(&b.route.service))
            });
        }
        Ok(Self {
            by_method,
            host_routes: kinds.host_routes().to_vec(),
        })
    }

    pub fn find_route(&self, path: &str, method: &str) -> Option<Matched> {
        let request: Vec<&str> = path.split('/').collect();
        self.by_method
            .get(method)?
            .iter()
            .find_map(|compiled| match_segments(compiled, &request))
    }

    /// Match a request using the `Host` header when it matches a root kind's
    /// subdomain template. On a tenant subdomain, kind routes are matched only
    /// under that tenant's resolved root prefix; `public` and `user` routes
    /// remain reachable as normal, while cross-tenant kind paths return `None`.
    pub fn find_route_with_host(
        &self,
        path: &str,
        method: &str,
        host: Option<&str>,
    ) -> Option<Matched> {
        if let Some((root_host, root_ref)) = host.and_then(|h| resolve_host(&self.host_routes, h)) {
            let synthetic = if path == "/" {
                format!("{}/{root_ref}", root_host.prefix)
            } else {
                format!("{}/{root_ref}{path}", root_host.prefix)
            };
            if let Some(matched) = self.find_route(&synthetic, method) {
                if matched.route.chain.first().map(String::as_str) == Some(root_host.kind.as_str())
                {
                    return Some(matched);
                }
            }
            if let Some(matched) = self.find_route(path, method) {
                if matches!(matched.route.scope, Scope::Public | Scope::User) {
                    return Some(matched);
                }
            }
            return None;
        }
        self.find_route(path, method)
    }

    pub fn route_count(&self) -> usize {
        self.by_method.values().map(Vec::len).sum()
    }

    /// Every route, in match order within each method.
    pub fn routes(&self) -> impl Iterator<Item = &Arc<Route>> {
        self.by_method.values().flatten().map(|c| &c.route)
    }
}

/// Literal before param, segment by segment: the order routes are tried in.
fn shape(segments: &[Segment]) -> Vec<bool> {
    segments
        .iter()
        .map(|s| !matches!(s, Segment::Literal(_)))
        .collect()
}

/// Pre-parsed rate-limit policy (window and tiers parsed once per snapshot).
pub struct CompiledPolicy {
    pub name: String,
    pub requests: u64,
    pub window_seconds: u64,
    pub count_at: Option<String>,
    pub tiers: BTreeMap<String, TierLimit>,
}

impl CompiledPolicy {
    pub fn compile(name: &str, doc: &PolicyDoc) -> Result<Self, String> {
        let window_seconds = parse_duration(&doc.window)
            .filter(|d| *d >= Duration::from_secs(1))
            .ok_or_else(|| {
                format!(
                    "rate_limit {name} has invalid window {:?} (must be at least 1s)",
                    doc.window
                )
            })?
            .as_secs();
        let tiers = parse_tiers(name, &doc.tiers)?;
        Ok(Self {
            name: name.to_string(),
            requests: doc.requests,
            window_seconds,
            count_at: doc.count_at.clone(),
            tiers,
        })
    }

    pub fn into_policy(&self, count_at: Option<usize>) -> Policy {
        Policy {
            name: self.name.clone(),
            requests: self.requests,
            window_seconds: self.window_seconds,
            count_at,
            tiers: self.tiers.clone(),
        }
    }
}

pub fn compile_policies(
    docs: &BTreeMap<String, PolicyDoc>,
) -> Result<BTreeMap<String, CompiledPolicy>, String> {
    docs.iter()
        .map(|(name, doc)| Ok((name.clone(), CompiledPolicy::compile(name, doc)?)))
        .collect()
}

fn match_segments(compiled: &CompiledRoute, request: &[&str]) -> Option<Matched> {
    if compiled.segments.len() != request.len() {
        return None;
    }
    // Check literals and non-empty param values before allocating.
    for (segment, value) in compiled.segments.iter().zip(request) {
        match segment {
            Segment::Literal(text) if text == value => {}
            Segment::Literal(_) => return None,
            _ if value.is_empty() => return None,
            _ => {}
        }
    }
    let mut refs = vec![String::new(); compiled.route.chain.len()];
    let mut params = BTreeMap::new();
    for (segment, value) in compiled.segments.iter().zip(request) {
        match segment {
            Segment::Chain(i) => refs[*i] = (*value).to_string(),
            Segment::Param(name) => {
                params.insert(name.clone(), (*value).to_string());
            }
            Segment::Literal(_) => {}
        }
    }
    Some(Matched {
        route: compiled.route.clone(),
        refs,
        params,
    })
}

fn build_route(
    service_name: &str,
    service: &ServiceDoc,
    timeouts: Timeouts,
    doc: &RouteDoc,
    rate_limits: &BTreeMap<String, CompiledPolicy>,
    kinds: &Kinds,
    authorizer: Option<&AuthorizerSpec>,
) -> Result<CompiledRoute, String> {
    let scope = match doc.scope.as_str() {
        "public" => Scope::Public,
        "user" => Scope::User,
        kind => Scope::Kind(kind.to_string()),
    };
    let chain = match &scope {
        Scope::Kind(kind) => kinds.chain(kind)?,
        _ => Vec::new(),
    };

    let segments = parse_path(&doc.path, &chain)?;
    let own_params: Vec<&str> = segments
        .iter()
        .filter_map(|s| match s {
            Segment::Param(name) => Some(name.as_str()),
            _ => None,
        })
        .collect();
    let upstream = Upstream::compile(&doc.upstream, &scope, &chain, &own_params)?;

    let required = match (&scope, &doc.permissions) {
        (Scope::Kind(_), Some(RequiredDoc::Permissions(list))) => Required::AnyOf(list.clone()),
        (Scope::Kind(_), Some(RequiredDoc::Word(w))) if w == "any_member" => Required::AnyMember,
        (Scope::Kind(_), other) => return Err(format!("permissions {other:?}")),
        _ => Required::Nothing,
    };

    let policies = doc
        .rate_limit
        .iter()
        .map(|name| {
            let policy = rate_limits
                .get(name)
                .ok_or_else(|| format!("rate_limit {name} is not a policy"))?;
            let count_at = match &policy.count_at {
                None => None,
                Some(kind) => Some(chain.iter().position(|k| k == kind).ok_or_else(|| {
                    format!("rate_limit {name} counts at {kind}, outside the chain")
                })?),
            };
            Ok(policy.into_policy(count_at))
        })
        .collect::<Result<Vec<_>, String>>()?;

    let resource = match &doc.resource {
        None => None,
        Some(r) => {
            if authorizer.is_none() {
                return Err("resource requires top-level authorizer".to_string());
            }
            if !matches!(&required, Required::AnyOf(list) if !list.is_empty()) {
                return Err("resource requires permissions to list at least one action".to_string());
            }
            Some(RouteResource {
                r#type: r.r#type.clone(),
                id: Upstream::compile(&r.id, &scope, &chain, &own_params)?,
            })
        }
    };

    Ok(CompiledRoute {
        segments,
        route: Arc::new(Route {
            service: service_name.to_string(),
            base_url: service.url.clone(),
            path: doc.path.clone(),
            method: doc.method.clone(),
            scope,
            chain,
            upstream,
            required,
            policies,
            audit: doc.audit,
            allow_deleted: doc.allow_deleted,
            resource,
            timeouts,
            max_body_bytes: service.max_body_bytes,
        }),
    })
}

/// A path's segments. Each `{param}` is a whole segment; one named for a chain
/// kind is that kind's param, and every chain kind has one.
fn parse_path(path: &str, chain: &[String]) -> Result<Vec<Segment>, String> {
    let mut segments = Vec::new();
    let mut seen = vec![false; chain.len()];
    let mut params: Vec<&str> = Vec::new();
    for segment in path.split('/') {
        match segment.strip_prefix('{').and_then(|s| s.strip_suffix('}')) {
            Some(name) => match chain.iter().position(|k| k == name) {
                Some(i) if !seen[i] => {
                    seen[i] = true;
                    segments.push(Segment::Chain(i));
                }
                None if !params.contains(&name) => {
                    params.push(name);
                    segments.push(Segment::Param(name.to_string()));
                }
                _ => return Err(format!("path param {{{name}}} twice")),
            },
            None if segment.contains(['{', '}']) => {
                return Err(format!("path segment {segment} is not one param"));
            }
            None => segments.push(Segment::Literal(segment.to_string())),
        }
    }
    if let Some(i) = seen.iter().position(|s| !s) {
        return Err(format!("no {{{}}} in the path", chain[i]));
    }
    Ok(segments)
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn kinds() -> Kinds {
        Kinds::from_feed(
            &serde_json::from_value(json!({
                "organization": { "path": "/orgs/{organization}" },
                "project": { "parent": "organization", "path": "/projects/{project}" }
            }))
            .unwrap(),
        )
    }

    fn route(path: &str, method: &str, scope: &str, upstream: &str) -> serde_json::Value {
        let required = match scope {
            "public" | "user" => json!(null),
            _ => json!(["x:read"]),
        };
        json!({ "path": path, "method": method, "scope": scope, "upstream": upstream,
                "permissions": required, "rate_limit": [], "audit": false,
                "allow_deleted": false })
    }

    fn service(routes: Vec<serde_json::Value>) -> serde_json::Value {
        json!({ "url": "http://svc:3000", "timeouts": { "connect": "2s", "read": "1m" },
                "max_body_bytes": 1024, "routes": routes })
    }

    fn build_with(
        services: serde_json::Value,
        rate_limits: serde_json::Value,
    ) -> Result<RouteMap, String> {
        let services: BTreeMap<String, ServiceDoc> = serde_json::from_value(services).unwrap();
        let rate_limits: BTreeMap<String, PolicyDoc> = serde_json::from_value(rate_limits).unwrap();
        RouteMap::from_feed(&services, &rate_limits, &kinds())
    }

    fn one(route: serde_json::Value) -> Result<RouteMap, String> {
        build_with(json!({ "svc": service(vec![route]) }), json!({}))
    }

    #[test]
    fn a_literal_beats_a_param_from_the_left() {
        let map = build_with(
            json!({
                "b": service(vec![
                    route("/api/{id}", "GET", "public", "/api/{id}"),
                    route("/a/{x}/c", "GET", "public", "/x"),
                ]),
                "a": service(vec![
                    route("/api/health", "GET", "public", "/health"),
                    route("/a/b/{y}", "GET", "public", "/y"),
                ]),
            }),
            json!({}),
        )
        .unwrap();
        let m = map.find_route("/api/health", "GET").unwrap();
        assert_eq!(m.route.service, "a");
        let m = map.find_route("/api/x", "GET").unwrap();
        assert_eq!(m.route.service, "b");
        assert_eq!(m.params["id"], "x");
        // Both shapes match; the literal in the second segment decides.
        assert_eq!(
            map.find_route("/a/b/c", "GET").unwrap().route.path,
            "/a/b/{y}"
        );
        assert!(map.find_route("/api/x", "POST").is_none());
        assert!(map.find_route("/api/", "GET").is_none());
        assert!(map.find_route("/api/x/", "GET").is_none());
    }

    #[test]
    fn a_kind_route_takes_its_chain_from_the_kinds() {
        let map = one(route(
            "/orgs/{organization}/projects/{project}/plans/{plan_id}",
            "GET",
            "project",
            "/projects/{project.id}/plans/{plan_id}",
        ))
        .unwrap();
        let m = map
            .find_route("/orgs/acme/projects/web/plans/pro", "GET")
            .unwrap();
        assert_eq!(m.route.scope, Scope::Kind("project".into()));
        assert_eq!(m.route.chain, vec!["organization", "project"]);
        assert_eq!(m.refs, vec!["acme", "web"]);
        assert_eq!(
            m.params,
            BTreeMap::from([("plan_id".to_string(), "pro".to_string())])
        );
        assert_eq!(m.route.required, Required::AnyOf(vec!["x:read".into()]));
        assert_eq!(
            m.route.timeouts,
            Timeouts {
                connect: Duration::from_secs(2),
                read: Duration::from_secs(60)
            }
        );
        assert_eq!(m.route.max_body_bytes, 1024);
    }

    #[test]
    fn policies_resolve_by_name_and_count_at_its_chain_position() {
        let mut r = route(
            "/orgs/{organization}/projects/{project}/check",
            "POST",
            "project",
            "/projects/{project.id}/check",
        );
        r["rate_limit"] = json!(["per-caller", "evals-org"]);
        let map = build_with(
            json!({ "svc": service(vec![r]) }),
            json!({
                "per-caller": { "requests": 100, "window": "1s" },
                "evals-org": { "requests": 300, "window": "1s", "count_at": "organization" }
            }),
        )
        .unwrap();
        let m = map.find_route("/orgs/a/projects/b/check", "POST").unwrap();
        assert_eq!(
            m.route.policies,
            vec![
                Policy {
                    name: "per-caller".into(),
                    requests: 100,
                    window_seconds: 1,
                    count_at: None,
                    tiers: BTreeMap::new(),
                },
                Policy {
                    name: "evals-org".into(),
                    requests: 300,
                    window_seconds: 1,
                    count_at: Some(0),
                    tiers: BTreeMap::new(),
                },
            ]
        );
    }

    #[test]
    fn policy_tiers_override_requests_and_window() {
        let mut r = route("/orgs/{organization}/x", "GET", "organization", "/x");
        r["rate_limit"] = json!(["tiered"]);
        let map = build_with(
            json!({ "svc": service(vec![r]) }),
            json!({
                "tiered": {
                    "requests": 10,
                    "window": "1m",
                    "tiers": {
                        "enterprise": { "requests": 1000, "window": "10s" }
                    }
                }
            }),
        )
        .unwrap();
        let m = map.find_route("/orgs/acme/x", "GET").unwrap();
        let policy = &m.route.policies[0];
        assert_eq!(policy.effective(None), (10, 60));
        assert_eq!(policy.effective(Some("unknown")), (10, 60));
        assert_eq!(policy.effective(Some("enterprise")), (1000, 10));

        // Invalid tier window is rejected.
        let mut bad = route("/orgs/{organization}/x", "GET", "organization", "/x");
        bad["rate_limit"] = json!(["tiered"]);
        let err = build_with(
            json!({ "svc": service(vec![bad]) }),
            json!({
                "tiered": {
                    "requests": 10,
                    "window": "1m",
                    "tiers": { "bad": { "requests": 5, "window": "0s" } }
                }
            }),
        )
        .err()
        .unwrap();
        assert!(err.contains("invalid window"), "{err}");
    }

    #[test]
    fn host_based_routing_resolves_root_and_blocks_cross_tenant_paths() {
        let kinds = Kinds::from_feed(
            &serde_json::from_value(json!({
                "organization": {
                    "path": "/orgs/{organization}",
                    "host": "{organization}.api.example.com"
                },
                "project": {
                    "parent": "organization",
                    "path": "/projects/{project}"
                }
            }))
            .unwrap(),
        );
        let services: BTreeMap<String, ServiceDoc> = serde_json::from_value(json!({
            "svc": service(vec![
                route("/orgs/{organization}", "GET", "organization", "/nodes/{organization.id}"),
                route("/orgs/{organization}/projects/{project}", "GET", "project", "/projects/{project.id}"),
                route("/user", "GET", "user", "/users/{user.id}"),
                route("/health", "GET", "public", "/health"),
            ])
        }))
        .unwrap();
        let map = RouteMap::from_feed(&services, &BTreeMap::new(), &kinds).unwrap();

        // Subdomain `/` resolves to the root route.
        let m = map
            .find_route_with_host("/", "GET", Some("acme.api.example.com"))
            .unwrap();
        assert_eq!(m.route.path, "/orgs/{organization}");
        assert_eq!(m.refs, vec!["acme"]);

        // Subdomain `/projects/web` resolves to the child route with `acme` as root ref.
        let m = map
            .find_route_with_host("/projects/web", "GET", Some("ACME.api.example.com:443"))
            .unwrap();
        assert_eq!(m.route.path, "/orgs/{organization}/projects/{project}");
        assert_eq!(m.refs, vec!["acme", "web"]);

        // Public and user routes remain accessible on tenant subdomains.
        assert_eq!(
            map.find_route_with_host("/user", "GET", Some("acme.api.example.com"))
                .unwrap()
                .route
                .path,
            "/user"
        );
        assert_eq!(
            map.find_route_with_host("/health", "GET", Some("acme.api.example.com"))
                .unwrap()
                .route
                .path,
            "/health"
        );

        // Cross-tenant path on a tenant subdomain MUST NOT match another org.
        assert!(map
            .find_route_with_host("/orgs/other", "GET", Some("acme.api.example.com"))
            .is_none());
        assert!(map
            .find_route_with_host(
                "/orgs/other/projects/web",
                "GET",
                Some("acme.api.example.com")
            )
            .is_none());

        // Path-based routing still works on the base API host.
        let m = map
            .find_route_with_host("/orgs/other", "GET", Some("api.example.com"))
            .unwrap();
        assert_eq!(m.refs, vec!["other"]);
    }

    #[test]
    fn refuses_what_it_cannot_build() {
        let mut any_member = route("/orgs/{organization}/x", "GET", "organization", "/x");
        any_member["permissions"] = json!("anyone");
        let mut unset = route("/orgs/{organization}/x", "GET", "organization", "/x");
        unset["permissions"] = json!(null);
        let mut unknown_policy = route("/x", "GET", "public", "/x");
        unknown_policy["rate_limit"] = json!(["nope"]);
        let mut bad_timeout = service(vec![route("/x", "GET", "public", "/x")]);
        bad_timeout["timeouts"]["read"] = json!("soon");
        for (svc, why) in [
            (
                service(vec![route("/x", "GET", "folder", "/x")]),
                "not a kind",
            ),
            (
                service(vec![route("/projects/{project}", "GET", "project", "/p")]),
                "no {organization}",
            ),
            (
                service(vec![route("/files/{name}.json", "GET", "public", "/f")]),
                "not one param",
            ),
            (
                service(vec![route("/a/{id}/b/{id}", "GET", "public", "/f")]),
                "twice",
            ),
            (
                service(vec![route(
                    "/orgs/{organization}",
                    "GET",
                    "organization",
                    "/p/{project.id}",
                )]),
                "placeholder",
            ),
            (
                service(vec![route("/x", "GET", "public", "/u/{user.id}")]),
                "placeholder",
            ),
            (service(vec![any_member]), "permissions"),
            (service(vec![unset]), "permissions"),
            (service(vec![unknown_policy]), "not a policy"),
            (bad_timeout, "timeouts.read"),
        ] {
            let err = build_with(json!({ "svc": svc.clone() }), json!({}))
                .err()
                .unwrap_or_else(|| panic!("built {svc}"));
            assert!(err.contains(why), "{svc}: {err}");
        }

        let mut org_wide = route("/user/x", "GET", "user", "/x");
        org_wide["rate_limit"] = json!(["org"]);
        let err = build_with(
            json!({ "svc": service(vec![org_wide]) }),
            json!({ "org": { "requests": 1, "window": "1s", "count_at": "organization" } }),
        )
        .err()
        .unwrap();
        assert!(err.contains("outside the chain"), "{err}");

        let mut zero_window = route("/x", "GET", "public", "/x");
        zero_window["rate_limit"] = json!(["zero"]);
        for window in ["0s", "0m"] {
            let err = build_with(
                json!({ "svc": service(vec![zero_window.clone()]) }),
                json!({ "zero": { "requests": 10, "window": window } }),
            )
            .err()
            .unwrap();
            assert!(err.contains("invalid window"), "{window}: {err}");
        }
    }
}
