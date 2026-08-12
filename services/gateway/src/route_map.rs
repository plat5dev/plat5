use std::collections::HashMap;
use std::collections::HashSet;

use regex::Regex;
use tracing::warn;

use crate::route_config::Config;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum RouteScope {
    Public,
    User,
    Organization,
}

#[derive(Clone)]
pub struct Route {
    pub base_url: String,
    pub path: String,
    pub methods: HashSet<String>,
    /// Optional path transform - if set, rewrite the upstream path
    pub transform_path: Option<String>,
    /// The auth scope for this route
    pub scope: RouteScope,
    /// Path param name for organization id (`organization` scope only)
    pub organization_param: Option<String>,
}

impl Route {
    /// Resolve the upstream path, applying transform if configured.
    /// Substitutes path params like {id} with their captured values.
    pub fn resolve_upstream_path(&self, path_params: &HashMap<String, String>) -> String {
        let template = self.transform_path.as_ref().unwrap_or(&self.path);
        let mut resolved = template.clone();
        for (key, value) in path_params {
            resolved = resolved.replace(&format!("{{{}}}", key), value);
        }
        resolved
    }
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
                (
                    service_config.public.as_ref(),
                    RouteScope::Public,
                    None::<String>,
                ),
                (
                    service_config.user.as_ref(),
                    RouteScope::User,
                    None::<String>,
                ),
                (
                    service_config.organization.as_ref(),
                    RouteScope::Organization,
                    service_config
                        .organization
                        .as_ref()
                        .and_then(|o| o.organization_param.clone()),
                ),
            ];

            for (scope_cfg, scope, org_param) in scopes {
                let Some(scope_cfg) = scope_cfg else {
                    continue;
                };
                for route_config in &scope_cfg.routes {
                    let methods: Vec<&str> =
                        route_config.methods.iter().map(|s| s.as_str()).collect();
                    let transform_path =
                        route_config.transform.as_ref().and_then(|t| t.path.clone());

                    if let Err(e) = route_map.add_route(
                        base_url,
                        &route_config.path,
                        &methods,
                        transform_path,
                        scope,
                        org_param.clone(),
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

    pub fn add_route(
        &mut self,
        base_url: &str,
        path: &str,
        methods: &[&str],
        transform_path: Option<String>,
        scope: RouteScope,
        organization_param: Option<String>,
    ) -> Result<(), String> {
        if scope == RouteScope::Organization {
            let param = organization_param
                .as_deref()
                .filter(|p| !p.is_empty())
                .ok_or_else(|| "organization route missing organization_param".to_string())?;
            let needle = format!("{{{param}}}");
            if !path.contains(&needle) {
                return Err(format!(
                    "organization route path must include path param {needle}"
                ));
            }
        }

        let re = path_to_regex(path).map_err(|e| e.to_string())?;
        let (static_segments, param_count) = path_specificity(path);

        let route = Route {
            base_url: base_url.to_string(),
            path: path.to_string(),
            methods: methods.iter().map(|m| m.to_string()).collect(),
            transform_path,
            scope,
            organization_param,
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
    use crate::route_config::{Config, RouteConfig, ScopeConfig, ServiceConfig};

    fn route(path: &str, methods: &[&str]) -> RouteConfig {
        RouteConfig {
            path: path.to_string(),
            methods: methods.iter().map(|m| m.to_string()).collect(),
            transform: None,
        }
    }

    #[test]
    fn specificity_prefers_static_over_param() {
        let mut services = HashMap::new();
        services.insert(
            "b".to_string(),
            ServiceConfig {
                url: "http://b".into(),
                public: Some(ScopeConfig {
                    route_prefix: None,
                    organization_param: None,
                    routes: vec![route("/api/{id}", &["GET"])],
                }),
                user: None,
                organization: None,
            },
        );
        services.insert(
            "a".to_string(),
            ServiceConfig {
                url: "http://a".into(),
                public: Some(ScopeConfig {
                    route_prefix: None,
                    organization_param: None,
                    routes: vec![route("/api/health", &["GET"])],
                }),
                user: None,
                organization: None,
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
                    public: Some(ScopeConfig {
                        route_prefix: None,
                        organization_param: None,
                        routes: vec![route(path, &["GET"])],
                    }),
                    user: None,
                    organization: None,
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
    fn org_route_requires_param_in_path() {
        let mut map = RouteMap::new();
        assert!(map
            .add_route(
                "http://x",
                "/api/widgets",
                &["GET"],
                None,
                RouteScope::Organization,
                Some("organization_id".into()),
            )
            .is_err());
        assert!(map
            .add_route(
                "http://x",
                "/api/orgs/{organization_id}/widgets",
                &["GET"],
                None,
                RouteScope::Organization,
                Some("organization_id".into()),
            )
            .is_ok());
        assert!(map
            .add_route(
                "http://x",
                "/api/orgs/{organization_id}",
                &["GET"],
                None,
                RouteScope::Organization,
                None,
            )
            .is_err());
    }
}
