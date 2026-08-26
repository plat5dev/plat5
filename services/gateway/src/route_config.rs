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
    #[allow(dead_code)] // serde match only; payload unread
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

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum RateLimitBy {
    Ip,
    User,
    Member,
}

impl RateLimitBy {
    pub fn parse(raw: &str) -> Option<Self> {
        match raw.trim() {
            "ip" => Some(Self::Ip),
            "user" => Some(Self::User),
            "member" => Some(Self::Member),
            _ => None,
        }
    }

    #[allow(dead_code)] // used by gateway; kept on both route_config copies
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Ip => "ip",
            Self::User => "user",
            Self::Member => "member",
        }
    }
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
