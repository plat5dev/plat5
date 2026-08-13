use serde::{Deserialize, Serialize};
use std::collections::HashMap;

/// etcd key prefix for gateway route registry entries.
pub const ROUTES_PREFIX: &str = "edge/gateway/routes/";

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
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct TransformConfig {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub path: Option<String>,
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
    }
    Ok(())
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
                routes: vec![RouteConfig {
                    path: "/".into(),
                    methods: vec!["GET".into()],
                    transform: None,
                }],
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
}
