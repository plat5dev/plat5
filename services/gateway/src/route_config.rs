use serde::{Deserialize, Serialize};
use std::collections::HashMap;

/// etcd key prefix for gateway route registry entries.
pub const ROUTES_PREFIX: &str = "edge/gateway/routes/";

pub const MAX_SCOPE_COUNT: usize = 32;
pub const MAX_SCOPE_LEN: usize = 64;

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

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct RouteConfig {
    pub path: String,
    pub methods: Vec<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub transform: Option<TransformConfig>,
    /// Optional API-key scope labels this route requires.
    /// Omitted = any admitted principal. JWTs and unrestricted keys skip the check.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub required_scopes: Option<Vec<String>>,
    /// Omitted = inherit gateway fallback. `false` = unlimited. Object = override.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub rate_limit: Option<RouteRateLimit>,
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
    fn deserialize<D: serde::Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        #[derive(Deserialize)]
        #[serde(untagged)]
        enum Raw {
            Flag(bool),
            Limit(RateLimitConfig),
        }
        match Raw::deserialize(deserializer)? {
            Raw::Flag(false) => Ok(RouteRateLimit::Unlimited),
            Raw::Flag(true) => Err(serde::de::Error::custom(
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
    /// Expand `route_prefix` into each route path and clear prefixes.
    /// Call before writing etcd (full paths only).
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
        Ok(())
    }

    /// Validate + expand a single service under `name` for registry write.
    pub fn prepare_for_registry(mut self, name: &str) -> Result<Self, ConfigError> {
        let check = Config {
            services: HashMap::from([(name.to_string(), self.clone())]),
        };
        check.validate()?;
        self.expand_route_prefixes()?;
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
        if route.methods.is_empty() {
            return Err(ConfigError::InvalidRoute {
                service: service.to_string(),
                reason: format!("{} route '{}' has no methods", scope_name, route.path),
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

        validate_required_scopes(service, scope_name, &route.path, route.required_scopes.as_deref())?;
        validate_rate_limit(service, scope_name, &route.path, route.rate_limit.as_ref())?;
    }
    Ok(())
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
        }
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
        services.insert(
            "w".into(),
            ServiceConfig {
                url: "w:3000".into(),
                public: None,
                user: Some(ScopeConfig {
                    route_prefix: None,
                    organization_param: None,
                    routes: vec![r],
                }),
                organization: None,
            },
        );
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
            services.insert(
                "w".into(),
                ServiceConfig {
                    url: "w:3000".into(),
                    public: None,
                    user: Some(ScopeConfig {
                        route_prefix: None,
                        organization_param: None,
                        routes: vec![r],
                    }),
                    organization: None,
                },
            );
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

        let obj: RouteRateLimit = serde_json::from_str(
            r#"{"requests":10,"window_seconds":60,"by":"ip"}"#,
        )
        .unwrap();
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
        services.insert(
            "w".into(),
            ServiceConfig {
                url: "w:3000".into(),
                public: None,
                user: Some(ScopeConfig {
                    route_prefix: None,
                    organization_param: None,
                    routes: vec![r],
                }),
                organization: None,
            },
        );
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
}
