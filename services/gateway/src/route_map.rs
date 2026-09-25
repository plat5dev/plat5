use std::collections::HashMap;
use std::collections::HashSet;

use regex::Regex;
use tracing::warn;

use crate::route_config::{Config, RateLimitPolicy, RouteRateLimit};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum RouteScope {
    Public,
    User,
    Organization,
    Member,
}

#[derive(Clone)]
pub struct Route {
    pub base_url: String,
    pub path: String,
    pub methods: HashSet<String>,
    /// Absolute upstream template. None means proxy `path` unchanged.
    pub upstream: Option<String>,
    /// The auth scope for this route
    pub scope: RouteScope,
    pub required_scopes: Option<Vec<String>>,
    pub limiter: RouteLimiter,
}

/// Resolved at load from `rate_limit` + service `rate_limits`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum RouteLimiter {
    /// Inherit gateway fallback (`RATE_LIMIT_*`).
    Inherit,
    Unlimited,
    Limited(ResolvedLimiter),
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ResolvedLimiter {
    pub requests: u64,
    pub window_seconds: u64,
    pub bucket: LimitBucket,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum LimitBucket {
    /// `{METHOD} {path} {subject}`
    MethodPath,
    /// `{prefix}:{subject}` (`service:name` or shared `name`)
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

pub struct RouteMap {
    routes: Vec<CompiledRoute>,
}

impl Default for RouteMap {
    fn default() -> Self {
        Self::new()
    }
}

impl RouteMap {
    pub fn new() -> Self {
        RouteMap { routes: Vec::new() }
    }

    /// Build RouteMap from parsed config. Skips routes whose paths contain
    /// invalid patterns and logs a warning.
    ///
    /// Services are processed in sorted name order. Routes are sorted by
    /// specificity (more static segments, fewer params, longer path, path string)
    /// so first-match is deterministic. Exact (path, method) duplicates are skipped
    /// after the first with a warning.
    pub fn from_config(config: &Config) -> Self {
        let mut route_map = RouteMap::new();

        let mut service_names: Vec<&String> = config.services.keys().collect();
        service_names.sort();

        for service_name in service_names {
            let service_config = &config.services[service_name];
            let base_url = &service_config.url;

            let scopes = [
                (service_config.public.as_ref(), RouteScope::Public),
                (service_config.user.as_ref(), RouteScope::User),
                (
                    service_config.organization.as_ref(),
                    RouteScope::Organization,
                ),
                (service_config.member.as_ref(), RouteScope::Member),
            ];

            for (scope_cfg, scope) in scopes {
                let Some(scope_cfg) = scope_cfg else {
                    continue;
                };
                for route_config in &scope_cfg.routes {
                    let methods: Vec<&str> =
                        route_config.methods.iter().map(|s| s.as_str()).collect();

                    let limiter = match bind_limiter(
                        service_name,
                        service_config.rate_limits.as_ref(),
                        route_config.rate_limit.as_ref(),
                    ) {
                        Ok(l) => l,
                        Err(e) => {
                            warn!(
                                service = %service_name,
                                path = %route_config.path,
                                error = %e,
                                "skipping invalid route"
                            );
                            continue;
                        }
                    };

                    if let Err(e) = route_map.add_route(
                        base_url,
                        &route_config.path,
                        &methods,
                        route_config.upstream.clone(),
                        scope,
                        route_config.required_scopes.clone(),
                        limiter,
                    ) {
                        warn!(
                            service = %service_name,
                            path = %route_config.path,
                            error = %e,
                            "skipping invalid route"
                        );
                    }
                }
            }
        }

        route_map.finalize();
        route_map
    }

    #[allow(clippy::too_many_arguments)]
    pub fn add_route(
        &mut self,
        base_url: &str,
        path: &str,
        methods: &[&str],
        upstream: Option<String>,
        scope: RouteScope,
        required_scopes: Option<Vec<String>>,
        limiter: RouteLimiter,
    ) -> Result<(), String> {
        let re = path_to_regex(path).map_err(|e| e.to_string())?;
        let (static_segments, param_count) = path_specificity(path);

        let route = Route {
            base_url: base_url.to_string(),
            path: path.to_string(),
            methods: methods.iter().map(|m| m.to_string()).collect(),
            upstream,
            scope,
            required_scopes,
            limiter,
        };

        self.routes.push(CompiledRoute {
            regex: re,
            route,
            static_segments,
            param_count,
        });
        Ok(())
    }

    /// Sort by specificity and drop exact (path, method) duplicates.
    fn finalize(&mut self) {
        self.routes.sort_by(|a, b| {
            b.static_segments
                .cmp(&a.static_segments)
                .then_with(|| a.param_count.cmp(&b.param_count))
                .then_with(|| b.route.path.len().cmp(&a.route.path.len()))
                .then_with(|| a.route.path.cmp(&b.route.path))
        });

        let mut seen: HashSet<(String, String)> = HashSet::new();
        let mut cleaned = Vec::with_capacity(self.routes.len());
        for mut compiled in self.routes.drain(..) {
            let methods: HashSet<String> = compiled
                .route
                .methods
                .iter()
                .filter(|method| {
                    let key = (compiled.route.path.clone(), (*method).clone());
                    if seen.insert(key) {
                        true
                    } else {
                        warn!(
                            path = %compiled.route.path,
                            method = %method,
                            "duplicate route path+method; keeping first (most specific / sorted)"
                        );
                        false
                    }
                })
                .cloned()
                .collect();
            if methods.is_empty() {
                continue;
            }
            compiled.route.methods = methods;
            cleaned.push(compiled);
        }
        self.routes = cleaned;
    }

    pub fn find_route(
        &self,
        external_path: &str,
        method: &str,
    ) -> Option<(&Route, HashMap<String, String>)> {
        for compiled in &self.routes {
            if let Some(caps) = compiled.regex.captures(external_path) {
                if !compiled.route.methods.contains(method) {
                    continue;
                }

                let mut path_params = HashMap::new();
                compiled.regex.capture_names().for_each(|key| {
                    if let Some(key) = key {
                        if let Some(value) = caps.name(key) {
                            path_params.insert(key.to_string(), value.as_str().to_string());
                        }
                    }
                });
                return Some((&compiled.route, path_params));
            }
        }
        None
    }

    pub fn route_count(&self) -> usize {
        self.routes.len()
    }
}

fn bind_limiter(
    service_name: &str,
    policies: Option<&HashMap<String, RateLimitPolicy>>,
    rate_limit: Option<&RouteRateLimit>,
) -> Result<RouteLimiter, String> {
    match rate_limit {
        None => Ok(RouteLimiter::Inherit),
        Some(RouteRateLimit::Unlimited) => Ok(RouteLimiter::Unlimited),
        Some(RouteRateLimit::Limit(cfg)) => Ok(RouteLimiter::Limited(ResolvedLimiter {
            requests: cfg.requests,
            window_seconds: cfg.window_seconds,
            bucket: LimitBucket::MethodPath,
        })),
        Some(RouteRateLimit::Named(name)) => {
            let policy = policies
                .and_then(|p| p.get(name))
                .ok_or_else(|| format!("rate_limit '{name}' is not defined on this service"))?;
            let prefix = if policy.shared {
                name.clone()
            } else {
                format!("{service_name}:{name}")
            };
            Ok(RouteLimiter::Limited(ResolvedLimiter {
                requests: policy.requests,
                window_seconds: policy.window_seconds,
                bucket: LimitBucket::Named(prefix),
            }))
        }
    }
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
    let mut chars = path.chars().peekable();

    while let Some(ch) = chars.next() {
        if ch == '{' {
            let mut name = String::new();
            for c in chars.by_ref() {
                if c == '}' {
                    break;
                }
                name.push(c);
            }
            if name.is_empty() {
                return Err(regex::Error::Syntax("empty path parameter name".into()));
            }
            pattern.push_str(&format!("(?P<{}>[^/]+)", regex::escape(&name)));
        } else {
            pattern.push_str(&regex::escape(&ch.to_string()));
        }
    }

    pattern.push('$');
    Regex::new(&pattern)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::route_config::{
        Config, RateLimitPolicy, RouteConfig, RouteRateLimit, ScopeConfig, ServiceConfig,
    };

    fn route(path: &str, methods: &[&str]) -> RouteConfig {
        RouteConfig {
            path: path.to_string(),
            methods: methods.iter().map(|m| m.to_string()).collect(),
            upstream: None,
            required_scopes: None,
            rate_limit: None,
            ..Default::default()
        }
    }

    #[test]
    fn specificity_prefers_static_over_param() {
        let mut services = HashMap::new();
        services.insert(
            "b".to_string(),
            ServiceConfig {
                url: "http://b".into(),
                rate_limits: None,
                public: Some(ScopeConfig {
                    route_prefix: None,
                    routes: vec![route("/api/{id}", &["GET"])],
                }),
                user: None,
                organization: None,
                member: None,
            },
        );
        services.insert(
            "a".to_string(),
            ServiceConfig {
                url: "http://a".into(),
                rate_limits: None,
                public: Some(ScopeConfig {
                    route_prefix: None,
                    routes: vec![route("/api/health", &["GET"])],
                }),
                user: None,
                organization: None,
                member: None,
            },
        );
        let map = RouteMap::from_config(&Config { services });
        let (r, _) = map.find_route("/api/health", "GET").unwrap();
        assert_eq!(r.base_url, "http://a");
        assert_eq!(r.path, "/api/health");
    }

    #[test]
    fn deterministic_service_order() {
        // Same specificity: path string order decides
        let mut services = HashMap::new();
        for (name, path, url) in [("z", "/x/z", "http://z"), ("a", "/x/a", "http://a")] {
            services.insert(
                name.to_string(),
                ServiceConfig {
                    url: url.into(),
                    rate_limits: None,
                    public: Some(ScopeConfig {
                        route_prefix: None,
                        routes: vec![route(path, &["GET"])],
                    }),
                    user: None,
                    organization: None,
                    member: None,
                },
            );
        }
        let map = RouteMap::from_config(&Config { services });
        assert_eq!(
            map.find_route("/x/a", "GET").unwrap().0.base_url,
            "http://a"
        );
        assert_eq!(
            map.find_route("/x/z", "GET").unwrap().0.base_url,
            "http://z"
        );
    }

    #[test]
    fn empty_param_name_rejected() {
        assert!(path_to_regex("/api/{}").is_err());
    }

    #[test]
    fn named_policy_binds_service_and_shared_buckets() {
        let mut local = route("/api/w", &["POST"]);
        local.rate_limit = Some(RouteRateLimit::Named("writes".into()));
        let mut shared = route("/api/s", &["DELETE"]);
        shared.rate_limit = Some(RouteRateLimit::Named("org-writes".into()));
        let mut services = HashMap::new();
        services.insert(
            "projects".into(),
            ServiceConfig {
                url: "http://p".into(),
                rate_limits: Some(HashMap::from([
                    (
                        "writes".into(),
                        RateLimitPolicy {
                            requests: 30,
                            window_seconds: 60,
                            shared: false,
                        },
                    ),
                    (
                        "org-writes".into(),
                        RateLimitPolicy {
                            requests: 10,
                            window_seconds: 60,
                            shared: true,
                        },
                    ),
                ])),
                public: None,
                user: Some(ScopeConfig {
                    route_prefix: None,
                    routes: vec![local, shared],
                }),
                organization: None,
                member: None,
            },
        );
        let map = RouteMap::from_config(&Config { services });
        let (w, _) = map.find_route("/api/w", "POST").unwrap();
        match &w.limiter {
            RouteLimiter::Limited(b) => {
                assert_eq!(b.requests, 30);
                assert_eq!(b.bucket, LimitBucket::Named("projects:writes".into()));
            }
            other => panic!("{other:?}"),
        }
        let (s, _) = map.find_route("/api/s", "DELETE").unwrap();
        match &s.limiter {
            RouteLimiter::Limited(b) => {
                assert_eq!(b.bucket, LimitBucket::Named("org-writes".into()));
            }
            other => panic!("{other:?}"),
        }
    }

    #[test]
    fn member_scope_route_loads() {
        let mut map = RouteMap::new();
        assert!(map
            .add_route(
                "http://x",
                "/member/api-keys",
                &["GET"],
                Some("/members/{subject.member_id}/api-keys".into()),
                RouteScope::Member,
                None,
                RouteLimiter::Inherit,
            )
            .is_ok());
        let (route, _) = map.find_route("/member/api-keys", "GET").unwrap();
        assert_eq!(route.scope, RouteScope::Member);
        assert_eq!(
            route.upstream.as_deref(),
            Some("/members/{subject.member_id}/api-keys")
        );
    }
}
