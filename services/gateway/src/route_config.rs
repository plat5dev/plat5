use serde::de::{self, Deserializer, IgnoredAny};
use serde::{Deserialize, Serialize};
use std::collections::{BTreeMap, HashMap, HashSet};

/// etcd key prefix for gateway route registry entries.
pub const ROUTES_PREFIX: &str = "edge/gateway/routes/";

pub const MAX_SCOPE_COUNT: usize = 32;
pub const MAX_SCOPE_LEN: usize = 64;

const HTTP_METHODS: &[&str] = &["GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"];

#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct Config {
    pub services: HashMap<String, ServiceConfig>,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct ServiceConfig {
    pub url: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub public: Option<ScopeConfig>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub user: Option<ScopeConfig>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub organization: Option<ScopeConfig>,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct ScopeConfig {
    /// Optional path prefix. Expanded into each route `path` at write time.
    /// Etcd stores full paths; gateway never requires this field.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub route_prefix: Option<String>,
    /// Required on `organization` scope — path param name for org id.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub organization_param: Option<String>,
    pub routes: Vec<RouteConfig>,
}

/// Apply-time nested method body. Empty (`GET:` / `GET: {}`) means that verb, no extra constraints.
#[derive(Clone, Debug, Default, Deserialize, Serialize, PartialEq)]
pub struct MethodConfig {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub required_scopes: Option<Vec<String>>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub rate_limit: Option<RouteRateLimit>,
}

#[derive(Clone, Debug, Default, PartialEq)]
pub(crate) enum MethodsForm {
    #[default]
    List,
    Nested(Vec<(String, MethodConfig)>),
    Mixed,
}

#[derive(Clone, Debug, Serialize)]
pub struct RouteConfig {
    pub path: String,
    pub methods: Vec<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub transform: Option<TransformConfig>,
    /// Optional API-key scope labels this route requires.
    /// Omitted = any admitted principal. JWTs and unrestricted keys skip the check.
    /// Route-level value applies only to the flat methods list form.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub required_scopes: Option<Vec<String>>,
    /// Omitted = inherit gateway fallback. `false` = unlimited. Object = override.
    /// Route-level value applies only to the flat methods list form.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub rate_limit: Option<RouteRateLimit>,
    /// Nested methods map, if the YAML/JSON used the per-verb object form.
    /// Cleared when expanded at apply. Never written to etcd.
    #[serde(skip)]
    pub(crate) methods_form: MethodsForm,
}

#[derive(Deserialize)]
#[serde(untagged)]
enum RawMethods {
    List(Vec<String>),
    Nested(BTreeMap<String, Option<MethodConfig>>),
    MixedSeq(Vec<IgnoredAny>),
}

#[derive(Deserialize)]
struct RawRouteConfig {
    path: String,
    methods: RawMethods,
    #[serde(default)]
    transform: Option<TransformConfig>,
    #[serde(default)]
    required_scopes: Option<Vec<String>>,
    #[serde(default)]
    rate_limit: Option<RouteRateLimit>,
}

impl<'de> Deserialize<'de> for RouteConfig {
    fn deserialize<D: Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        let raw = RawRouteConfig::deserialize(deserializer)?;
        match raw.methods {
            RawMethods::List(methods) => Ok(RouteConfig {
                path: raw.path,
                methods,
                transform: raw.transform,
                required_scopes: raw.required_scopes,
                rate_limit: raw.rate_limit,
                methods_form: MethodsForm::List,
            }),
            RawMethods::Nested(map) => {
                if map.is_empty() {
                    return Ok(RouteConfig {
                        path: raw.path,
                        methods: Vec::new(),
                        transform: raw.transform,
                        required_scopes: raw.required_scopes,
                        rate_limit: raw.rate_limit,
                        methods_form: MethodsForm::Nested(Vec::new()),
                    });
                }
                let entries: Vec<(String, MethodConfig)> = map
                    .into_iter()
                    .map(|(k, v)| (k, v.unwrap_or_default()))
                    .collect();
                let methods = entries.iter().map(|(m, _)| m.clone()).collect();
                Ok(RouteConfig {
                    path: raw.path,
                    methods,
                    transform: raw.transform,
                    required_scopes: raw.required_scopes,
                    rate_limit: raw.rate_limit,
                    methods_form: MethodsForm::Nested(entries),
                })
            }
            RawMethods::MixedSeq(_) => Ok(RouteConfig {
                path: raw.path,
                methods: Vec::new(),
                transform: raw.transform,
                required_scopes: raw.required_scopes,
                rate_limit: raw.rate_limit,
                methods_form: MethodsForm::Mixed,
            }),
        }
    }
}

impl Default for RouteConfig {
    fn default() -> Self {
        Self {
            path: String::new(),
            methods: Vec::new(),
            transform: None,
            required_scopes: None,
            rate_limit: None,
            methods_form: MethodsForm::List,
        }
    }
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct TransformConfig {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub path: Option<String>,
}

/// Per-route rate limit. `false` opts out (unlimited). Object overrides the gateway fallback.
#[derive(Clone, Debug, PartialEq)]
pub enum RouteRateLimit {
    Unlimited,
    Limit(RateLimitConfig),
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
pub struct RateLimitConfig {
    pub requests: u64,
    pub window_seconds: u64,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub by: Option<String>,
}

impl Serialize for RouteRateLimit {
    fn serialize<S: serde::Serializer>(&self, serializer: S) -> Result<S::Ok, S::Error> {
        match self {
            RouteRateLimit::Unlimited => serializer.serialize_bool(false),
            RouteRateLimit::Limit(cfg) => cfg.serialize(serializer),
        }
    }
}

impl<'de> Deserialize<'de> for RouteRateLimit {
    fn deserialize<D: Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        #[derive(Deserialize)]
        #[serde(untagged)]
        enum Raw {
            Flag(bool),
            Limit(RateLimitConfig),
        }
        match Raw::deserialize(deserializer)? {
            Raw::Flag(false) => Ok(RouteRateLimit::Unlimited),
            Raw::Flag(true) => Err(de::Error::custom(
                "rate_limit: true is invalid; use false or {requests, window_seconds, by?}",
            )),
            Raw::Limit(cfg) => Ok(RouteRateLimit::Limit(cfg)),
        }
    }
}

impl Config {
    /// Validate config is well-formed (source YAML or etcd JSON).
    pub fn validate(&self) -> Result<(), ConfigError> {
        for (name, service) in &self.services {
            if service.public.is_none() && service.user.is_none() && service.organization.is_none()
            {
                return Err(ConfigError::InvalidRoute {
                    service: name.clone(),
                    reason: "service has no routes (public, user, or organization)".to_string(),
                });
            }

            if let Some(ref public) = service.public {
                validate_scope_routes(name, "public", public, None)?;
            }
            if let Some(ref user) = service.user {
                validate_scope_routes(name, "user", user, None)?;
            }
            if let Some(ref org) = service.organization {
                let param =
                    org.organization_param
                        .as_deref()
                        .ok_or_else(|| ConfigError::InvalidRoute {
                            service: name.clone(),
                            reason: "organization scope requires organization_param".to_string(),
                        })?;
                if param.is_empty() {
                    return Err(ConfigError::InvalidRoute {
                        service: name.clone(),
                        reason: "organization_param must not be empty".to_string(),
                    });
                }
                validate_scope_routes(name, "organization", org, Some(param))?;
            }
        }
        Ok(())
    }
}

impl ServiceConfig {
    /// Expand `route_prefix` into each route path and expand nested methods
    /// into one `RouteConfig` row per verb. Call before writing etcd
    /// (full paths only; `methods` always a string array).
    pub fn expand_route_prefixes(&mut self) -> Result<(), ConfigError> {
        if let Some(ref mut public) = self.public {
            expand_scope(public)?;
        }
        if let Some(ref mut user) = self.user {
            expand_scope(user)?;
        }
        if let Some(ref mut organization) = self.organization {
            expand_scope(organization)?;
        }
        reject_duplicate_path_methods("", self)?;
        Ok(())
    }

    /// Validate + expand a single service under `name` for registry write.
    pub fn prepare_for_registry(mut self, name: &str) -> Result<Self, ConfigError> {
        let check = Config {
            services: HashMap::from([(name.to_string(), self.clone())]),
        };
        check.validate()?;
        self.expand_route_prefixes().map_err(|e| match e {
            ConfigError::InvalidRoute { reason, .. } => ConfigError::InvalidRoute {
                service: name.to_string(),
                reason,
            },
        })?;
        let check = Config {
            services: HashMap::from([(name.to_string(), self.clone())]),
        };
        check.validate()?;
        Ok(self)
    }
}

fn expand_scope(scope: &mut ScopeConfig) -> Result<(), ConfigError> {
    if let Some(prefix) = scope.route_prefix.take() {
        for route in &mut scope.routes {
            route.path = join_route_prefix(&prefix, &route.path).map_err(|reason| {
                ConfigError::InvalidRoute {
                    service: String::new(),
                    reason,
                }
            })?;
        }
    }
    expand_nested_methods(scope);
    Ok(())
}

fn expand_nested_methods(scope: &mut ScopeConfig) {
    let mut out = Vec::with_capacity(scope.routes.len());
    for mut route in scope.routes.drain(..) {
        let form = std::mem::replace(&mut route.methods_form, MethodsForm::List);
        match form {
            MethodsForm::Nested(entries) => {
                for (method, spec) in entries {
                    out.push(RouteConfig {
                        path: route.path.clone(),
                        methods: vec![method],
                        transform: route.transform.clone(),
                        required_scopes: spec.required_scopes,
                        rate_limit: spec.rate_limit,
                        methods_form: MethodsForm::List,
                    });
                }
            }
            _ => out.push(route),
        }
    }
    scope.routes = out;
}

fn reject_duplicate_path_methods(service: &str, svc: &ServiceConfig) -> Result<(), ConfigError> {
    let mut seen: HashSet<(String, String)> = HashSet::new();
    let scopes = [
        svc.public.as_ref(),
        svc.user.as_ref(),
        svc.organization.as_ref(),
    ];
    for scope in scopes.into_iter().flatten() {
        for route in &scope.routes {
            for method in &route.methods {
                if !seen.insert((route.path.clone(), method.clone())) {
                    return Err(ConfigError::InvalidRoute {
                        service: service.to_string(),
                        reason: format!("duplicate path '{}' method '{}'", route.path, method),
                    });
                }
            }
        }
    }
    Ok(())
}

/// Join rule: path must be `/` (exactly the prefix) or start with `/`.
/// `path == "/"` → prefix with trailing slashes stripped (no forced trailing slash).
pub fn join_route_prefix(prefix: &str, path: &str) -> Result<String, String> {
    if path.is_empty() {
        return Err("route path is empty".to_string());
    }
    if path != "/" && !path.starts_with('/') {
        return Err(format!(
            "route path '{}' must be '/' or start with '/'",
            path
        ));
    }
    let base = prefix.trim_end_matches('/');
    if path == "/" {
        if base.is_empty() {
            return Ok("/".to_string());
        }
        return Ok(base.to_string());
    }
    Ok(format!("{}{}", base, path))
}

fn validate_scope_routes(
    service: &str,
    scope_name: &str,
    scope: &ScopeConfig,
    organization_param: Option<&str>,
) -> Result<(), ConfigError> {
    for route in &scope.routes {
        if route.path.is_empty() {
            return Err(ConfigError::InvalidRoute {
                service: service.to_string(),
                reason: format!("{} route path is empty", scope_name),
            });
        }
        if matches!(route.methods_form, MethodsForm::Mixed) {
            return Err(ConfigError::InvalidRoute {
                service: service.to_string(),
                reason: format!(
                    "{} route '{}' must not mix methods list and map",
                    scope_name, route.path
                ),
            });
        }

        if let Some(ref prefix) = scope.route_prefix {
            join_route_prefix(prefix, &route.path).map_err(|reason| ConfigError::InvalidRoute {
                service: service.to_string(),
                reason: format!("{}: {}", scope_name, reason),
            })?;
        }

        if let Some(param) = organization_param {
            let needle = format!("{{{}}}", param);
            let expanded = if let Some(ref prefix) = scope.route_prefix {
                join_route_prefix(prefix, &route.path).unwrap_or_else(|_| route.path.clone())
            } else {
                route.path.clone()
            };
            if !expanded.contains(&needle) {
                return Err(ConfigError::InvalidRoute {
                    service: service.to_string(),
                    reason: format!(
                        "organization route '{}' must include path param {{{}}}",
                        expanded, param
                    ),
                });
            }
        }

        match &route.methods_form {
            MethodsForm::Nested(entries) => {
                if entries.is_empty() {
                    return Err(ConfigError::InvalidRoute {
                        service: service.to_string(),
                        reason: format!(
                            "{} route '{}' methods map must not be empty",
                            scope_name, route.path
                        ),
                    });
                }
                for (method, spec) in entries {
                    if !valid_http_method(method) {
                        return Err(ConfigError::InvalidRoute {
                            service: service.to_string(),
                            reason: format!(
                                "{} route '{}' method '{}' is invalid (expected GET, POST, PUT, PATCH, DELETE, HEAD, or OPTIONS)",
                                scope_name, route.path, method
                            ),
                        });
                    }
                    validate_required_scopes(
                        service,
                        scope_name,
                        &route.path,
                        spec.required_scopes.as_deref(),
                    )?;
                    validate_rate_limit(
                        service,
                        scope_name,
                        &route.path,
                        spec.rate_limit.as_ref(),
                    )?;
                }
            }
            MethodsForm::List => {
                if route.methods.is_empty() {
                    return Err(ConfigError::InvalidRoute {
                        service: service.to_string(),
                        reason: format!("{} route '{}' has no methods", scope_name, route.path),
                    });
                }
                validate_required_scopes(
                    service,
                    scope_name,
                    &route.path,
                    route.required_scopes.as_deref(),
                )?;
                validate_rate_limit(service, scope_name, &route.path, route.rate_limit.as_ref())?;
            }
            MethodsForm::Mixed => {}
        }
    }
    Ok(())
}

fn valid_http_method(m: &str) -> bool {
    HTTP_METHODS.contains(&m)
}

fn validate_required_scopes(
    service: &str,
    scope_name: &str,
    path: &str,
    required: Option<&[String]>,
) -> Result<(), ConfigError> {
    let Some(labels) = required else {
        return Ok(());
    };
    if labels.is_empty() {
        return Err(ConfigError::InvalidRoute {
            service: service.to_string(),
            reason: format!(
                "{} route '{}' required_scopes must be omitted or a non-empty list",
                scope_name, path
            ),
        });
    }
    if labels.len() > MAX_SCOPE_COUNT {
        return Err(ConfigError::InvalidRoute {
            service: service.to_string(),
            reason: format!(
                "{} route '{}' required_scopes has more than {} labels",
                scope_name, path, MAX_SCOPE_COUNT
            ),
        });
    }
    let mut seen = HashMap::new();
    for label in labels {
        if !valid_scope_label(label) {
            return Err(ConfigError::InvalidRoute {
                service: service.to_string(),
                reason: format!(
                    "{} route '{}' required_scopes label '{}' is invalid (expected [a-z0-9:._-]+, max {})",
                    scope_name, path, label, MAX_SCOPE_LEN
                ),
            });
        }
        if seen.insert(label, ()).is_some() {
            return Err(ConfigError::InvalidRoute {
                service: service.to_string(),
                reason: format!(
                    "{} route '{}' required_scopes has duplicate label '{}'",
                    scope_name, path, label
                ),
            });
        }
    }
    Ok(())
}

fn validate_rate_limit(
    service: &str,
    scope_name: &str,
    path: &str,
    rate_limit: Option<&RouteRateLimit>,
) -> Result<(), ConfigError> {
    let Some(spec) = rate_limit else {
        return Ok(());
    };
    let RouteRateLimit::Limit(cfg) = spec else {
        return Ok(());
    };
    if cfg.requests == 0 {
        return Err(ConfigError::InvalidRoute {
            service: service.to_string(),
            reason: format!(
                "{} route '{}' rate_limit.requests must be > 0 (use rate_limit: false for unlimited)",
                scope_name, path
            ),
        });
    }
    if cfg.window_seconds == 0 {
        return Err(ConfigError::InvalidRoute {
            service: service.to_string(),
            reason: format!(
                "{} route '{}' rate_limit.window_seconds must be > 0",
                scope_name, path
            ),
        });
    }
    if let Some(ref by) = cfg.by {
        if !matches!(by.as_str(), "ip" | "user" | "member") {
            return Err(ConfigError::InvalidRoute {
                service: service.to_string(),
                reason: format!(
                    "{} route '{}' rate_limit.by must be ip, user, or member",
                    scope_name, path
                ),
            });
        }
    }
    Ok(())
}

pub fn valid_scope_label(s: &str) -> bool {
    if s.is_empty() || s.len() > MAX_SCOPE_LEN {
        return false;
    }
    s.chars()
        .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || matches!(c, ':' | '.' | '_' | '-'))
}

pub fn scopes_intersect(required: &[String], granted: &[String]) -> bool {
    granted.iter().any(|g| required.iter().any(|r| r == g))
}

#[derive(Debug)]
pub enum ConfigError {
    InvalidRoute { service: String, reason: String },
}

impl std::fmt::Display for ConfigError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            ConfigError::InvalidRoute { service, reason } => {
                if service.is_empty() {
                    write!(f, "invalid route: {}", reason)
                } else {
                    write!(f, "invalid route for service '{}': {}", service, reason)
                }
            }
        }
    }
}

impl std::error::Error for ConfigError {}

#[cfg(test)]
mod tests {
    use super::*;

    fn route(path: &str, methods: &[&str]) -> RouteConfig {
        RouteConfig {
            path: path.to_string(),
            methods: methods.iter().map(|m| m.to_string()).collect(),
            transform: None,
            required_scopes: None,
            rate_limit: None,
            methods_form: MethodsForm::List,
        }
    }

    fn user_service(routes: Vec<RouteConfig>) -> ServiceConfig {
        ServiceConfig {
            url: "w:3000".into(),
            public: None,
            user: Some(ScopeConfig {
                route_prefix: None,
                organization_param: None,
                routes,
            }),
            organization: None,
        }
    }

    fn parse_route(json: &str) -> RouteConfig {
        serde_json::from_str(json).expect("route json")
    }

    #[test]
    fn join_prefix_root_path() {
        assert_eq!(
            join_route_prefix("/api/organizations", "/").unwrap(),
            "/api/organizations"
        );
    }

    #[test]
    fn join_prefix_subpath() {
        assert_eq!(
            join_route_prefix("/api/organizations", "/{organization_id}").unwrap(),
            "/api/organizations/{organization_id}"
        );
    }

    #[test]
    fn prepare_expands_prefix() {
        let svc = ServiceConfig {
            url: "orgs:3000".into(),
            public: None,
            user: Some(ScopeConfig {
                route_prefix: Some("/api/organizations".into()),
                organization_param: None,
                routes: vec![route("/", &["GET"])],
            }),
            organization: None,
        };
        let prepared = svc.prepare_for_registry("organizations").unwrap();
        assert!(prepared.user.as_ref().unwrap().route_prefix.is_none());
        assert_eq!(
            prepared.user.as_ref().unwrap().routes[0].path,
            "/api/organizations"
        );
    }

    #[test]
    fn required_scopes_ok() {
        let mut r = route("/api/widgets", &["GET"]);
        r.required_scopes = Some(vec!["widgets:read".into(), "invoices.write".into()]);
        let mut services = HashMap::new();
        services.insert("w".into(), user_service(vec![r]));
        Config { services }.validate().unwrap();
    }

    #[test]
    fn required_scopes_reject_empty_and_bad() {
        for labels in [
            vec![],
            vec!["Widgets:read".into()],
            vec!["bad/scope".into()],
            vec!["ok".into(), "ok".into()],
        ] {
            let mut r = route("/api/widgets", &["GET"]);
            r.required_scopes = Some(labels.clone());
            let mut services = HashMap::new();
            services.insert("w".into(), user_service(vec![r]));
            assert!(
                Config { services }.validate().is_err(),
                "expected error for {labels:?}"
            );
        }
    }

    #[test]
    fn rate_limit_false_and_object() {
        let unlimited: RouteRateLimit = serde_json::from_str("false").unwrap();
        assert_eq!(unlimited, RouteRateLimit::Unlimited);

        let obj: RouteRateLimit =
            serde_json::from_str(r#"{"requests":10,"window_seconds":60,"by":"ip"}"#).unwrap();
        match obj {
            RouteRateLimit::Limit(cfg) => {
                assert_eq!(cfg.requests, 10);
                assert_eq!(cfg.window_seconds, 60);
                assert_eq!(cfg.by.as_deref(), Some("ip"));
            }
            RouteRateLimit::Unlimited => panic!("expected object"),
        }

        assert!(serde_json::from_str::<RouteRateLimit>("true").is_err());
    }

    #[test]
    fn rate_limit_object_must_be_positive() {
        let mut r = route("/api/widgets", &["GET"]);
        r.rate_limit = Some(RouteRateLimit::Limit(RateLimitConfig {
            requests: 0,
            window_seconds: 60,
            by: None,
        }));
        let mut services = HashMap::new();
        services.insert("w".into(), user_service(vec![r]));
        assert!(Config { services }.validate().is_err());
    }

    #[test]
    fn scopes_intersect_nonempty() {
        assert!(scopes_intersect(
            &["a".into(), "b".into()],
            &["b".into(), "c".into()]
        ));
        assert!(!scopes_intersect(&["a".into()], &[]));
        assert!(!scopes_intersect(&["a".into()], &["b".into()]));
    }

    #[test]
    fn flat_methods_list_still_validates() {
        let r = parse_route(
            r#"{"path":"/features","methods":["GET","POST"],"required_scopes":["org:read"]}"#,
        );
        assert_eq!(r.methods, vec!["GET", "POST"]);
        assert_eq!(
            r.required_scopes.as_deref(),
            Some(["org:read".to_string()].as_slice())
        );
        assert!(matches!(r.methods_form, MethodsForm::List));
        let mut services = HashMap::new();
        services.insert("w".into(), user_service(vec![r]));
        Config { services }.validate().unwrap();
    }

    #[test]
    fn nested_get_post_different_scopes_expands_to_two_routes() {
        let r = parse_route(
            r#"{
                "path": "/features",
                "methods": {
                    "GET": {"required_scopes": ["org:read"]},
                    "POST": {
                        "required_scopes": ["org:write"],
                        "rate_limit": {"requests": 100, "window_seconds": 1, "by": "ip"}
                    }
                }
            }"#,
        );
        let prepared = user_service(vec![r])
            .prepare_for_registry("w")
            .expect("expand nested methods");
        let routes = &prepared.user.as_ref().unwrap().routes;
        assert_eq!(routes.len(), 2);

        let get = routes
            .iter()
            .find(|rt| rt.methods == ["GET"])
            .expect("GET row");
        assert_eq!(get.path, "/features");
        assert_eq!(
            get.required_scopes.as_deref(),
            Some(["org:read".to_string()].as_slice())
        );
        assert!(get.rate_limit.is_none());
        assert!(matches!(get.methods_form, MethodsForm::List));

        let post = routes
            .iter()
            .find(|rt| rt.methods == ["POST"])
            .expect("POST row");
        assert_eq!(
            post.required_scopes.as_deref(),
            Some(["org:write".to_string()].as_slice())
        );
        match &post.rate_limit {
            Some(RouteRateLimit::Limit(cfg)) => {
                assert_eq!(cfg.requests, 100);
                assert_eq!(cfg.window_seconds, 1);
                assert_eq!(cfg.by.as_deref(), Some("ip"));
            }
            other => panic!("expected POST rate limit, got {other:?}"),
        }

        let json = serde_json::to_value(get).unwrap();
        assert!(json["methods"].is_array(), "etcd methods must stay a list");
        assert!(json.get("methods_form").is_none());
    }

    #[test]
    fn mixing_list_and_map_fails() {
        let r = parse_route(r#"{"path":"/features","methods":["GET",{"POST":{}}]}"#);
        let mut services = HashMap::new();
        services.insert("w".into(), user_service(vec![r]));
        let err = Config { services }.validate().unwrap_err();
        let msg = err.to_string();
        assert!(msg.contains("mix"), "expected mix error, got {msg}");
    }

    #[test]
    fn empty_nested_methods_map_fails() {
        let r = parse_route(r#"{"path":"/features","methods":{}}"#);
        let mut services = HashMap::new();
        services.insert("w".into(), user_service(vec![r]));
        let err = Config { services }.validate().unwrap_err();
        let msg = err.to_string();
        assert!(
            msg.contains("methods map must not be empty") || msg.contains("no methods"),
            "expected empty map error, got {msg}"
        );
    }

    #[test]
    fn nested_method_empty_body_ok() {
        let r = parse_route(r#"{"path":"/features","methods":{"GET":null,"POST":{}}}"#);
        let prepared = user_service(vec![r])
            .prepare_for_registry("w")
            .expect("empty method body is valid");
        let routes = &prepared.user.as_ref().unwrap().routes;
        assert_eq!(routes.len(), 2);
        for rt in routes {
            assert!(rt.required_scopes.is_none());
            assert!(rt.rate_limit.is_none());
            assert_eq!(rt.methods.len(), 1);
        }
    }

    #[test]
    fn duplicate_path_method_after_expand_fails() {
        let nested = parse_route(
            r#"{"path":"/features","methods":{"GET":{"required_scopes":["org:read"]}}}"#,
        );
        let flat = parse_route(r#"{"path":"/features","methods":["GET"]}"#);
        let err = user_service(vec![nested, flat])
            .prepare_for_registry("w")
            .unwrap_err();
        let msg = err.to_string();
        assert!(
            msg.contains("duplicate"),
            "expected duplicate path+method, got {msg}"
        );
    }
}
