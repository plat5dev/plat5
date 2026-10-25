//! The routes the gateway serves, built from the control plane's gateway feed
//! (docs/reference/api/internal.md#control-plane-feeds): one route per method,
//! with its upstream, permissions, policies and audit already filled in. The
//! control plane validated the config; the gateway parses it and builds its
//! table. A snapshot it can't build is refused whole.

use std::collections::{BTreeMap, HashMap};
use std::sync::Arc;
use std::time::Duration;

use serde::Deserialize;

use crate::config::parse_duration;
use crate::kinds::Kinds;
use crate::pipeline::Upstream;

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
    pub required_permissions: Option<RequiredDoc>,
    pub rate_limit: Vec<String>,
    pub audit: bool,
    pub allow_deleted: bool,
}

/// A list of permissions, or `"any_member"`.
#[derive(Clone, Debug, Deserialize)]
#[serde(untagged)]
pub enum RequiredDoc {
    Permissions(Vec<String>),
    Word(String),
}

/// A rate-limit policy from the feed's `rate_limits`.
#[derive(Clone, Debug, Deserialize)]
pub struct PolicyDoc {
    pub requests: u64,
    pub window_seconds: u64,
    #[serde(default)]
    pub count_at: Option<String>,
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

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Policy {
    pub name: String,
    pub requests: u64,
    pub window_seconds: u64,
    /// The chain position of the `count_at` kind. None = each caller at the route's node.
    pub count_at: Option<usize>,
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
}

impl RouteMap {
    /// Build from the feed. Err names the first route that can't be built: the
    /// caller refuses the whole snapshot.
    pub fn from_feed(
        services: &BTreeMap<String, ServiceDoc>,
        rate_limits: &BTreeMap<String, PolicyDoc>,
        kinds: &Kinds,
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
                let compiled = build_route(name, service, timeouts, doc, rate_limits, kinds)
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
        Ok(Self { by_method })
    }

    pub fn find_route(&self, path: &str, method: &str) -> Option<Matched> {
        let request: Vec<&str> = path.split('/').collect();
        self.by_method
            .get(method)?
            .iter()
            .find_map(|compiled| match_segments(compiled, &request))
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

fn match_segments(compiled: &CompiledRoute, request: &[&str]) -> Option<Matched> {
    if compiled.segments.len() != request.len() {
        return None;
    }
    let mut refs = vec![String::new(); compiled.route.chain.len()];
    let mut params = BTreeMap::new();
    for (segment, value) in compiled.segments.iter().zip(request) {
        match segment {
            Segment::Literal(text) if text == value => {}
            Segment::Literal(_) => return None,
            _ if value.is_empty() => return None,
            Segment::Chain(i) => refs[*i] = value.to_string(),
            Segment::Param(name) => {
                params.insert(name.clone(), value.to_string());
            }
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
    rate_limits: &BTreeMap<String, PolicyDoc>,
    kinds: &Kinds,
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

    let required = match (&scope, &doc.required_permissions) {
        (Scope::Kind(_), Some(RequiredDoc::Permissions(list))) => Required::AnyOf(list.clone()),
        (Scope::Kind(_), Some(RequiredDoc::Word(w))) if w == "any_member" => Required::AnyMember,
        (Scope::Kind(_), other) => return Err(format!("required_permissions {other:?}")),
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
            Ok(Policy {
                name: name.clone(),
                requests: policy.requests,
                window_seconds: policy.window_seconds,
                count_at,
            })
        })
        .collect::<Result<Vec<_>, String>>()?;

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
                "required_permissions": required, "rate_limit": [], "audit": false,
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
                "per-caller": { "requests": 100, "window_seconds": 1 },
                "evals-org": { "requests": 300, "window_seconds": 1, "count_at": "organization" }
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
                    count_at: None
                },
                Policy {
                    name: "evals-org".into(),
                    requests: 300,
                    window_seconds: 1,
                    count_at: Some(0)
                },
            ]
        );
    }

    #[test]
    fn refuses_what_it_cannot_build() {
        let mut any_member = route("/orgs/{organization}/x", "GET", "organization", "/x");
        any_member["required_permissions"] = json!("anyone");
        let mut unset = route("/orgs/{organization}/x", "GET", "organization", "/x");
        unset["required_permissions"] = json!(null);
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
            (service(vec![any_member]), "required_permissions"),
            (service(vec![unset]), "required_permissions"),
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
            json!({ "org": { "requests": 1, "window_seconds": 1, "count_at": "organization" } }),
        )
        .err()
        .unwrap();
        assert!(err.contains("outside the chain"), "{err}");
    }
}
