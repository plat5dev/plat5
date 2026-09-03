fn validate_scope_routes(
    service: &str,
    scope_name: &str,
    scope: &ScopeConfig,
    organization_param: Option<&str>,
    policies: Option<&HashMap<String, RateLimitPolicy>>,
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
                    if !HTTP_METHODS.contains(&method.as_str()) {
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
                        policies,
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
                validate_rate_limit(
                    service,
                    scope_name,
                    &route.path,
                    route.rate_limit.as_ref(),
                    policies,
                )?;
            }
            MethodsForm::Mixed => {}
        }
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
    policies: Option<&HashMap<String, RateLimitPolicy>>,
) -> Result<(), ConfigError> {
    let Some(spec) = rate_limit else {
        return Ok(());
    };
    match spec {
        RouteRateLimit::Unlimited => Ok(()),
        RouteRateLimit::Limit(cfg) => {
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
            Ok(())
        }
        RouteRateLimit::Named(name) => {
            if !valid_scope_label(name) {
                return Err(ConfigError::InvalidRoute {
                    service: service.to_string(),
                    reason: format!(
                        "{} route '{}' rate_limit policy name '{}' is invalid (expected [a-z0-9:._-]+, max {})",
                        scope_name, path, name, MAX_SCOPE_LEN
                    ),
                });
            }
            match policies.and_then(|p| p.get(name)) {
                Some(_) => Ok(()),
                None => Err(ConfigError::InvalidRoute {
                    service: service.to_string(),
                    reason: format!(
                        "{} route '{}' rate_limit '{}' is not defined on this service",
                        scope_name, path, name
                    ),
                }),
            }
        }
    }
}

fn validate_rate_limit_policies(
    service: &str,
    policies: Option<&HashMap<String, RateLimitPolicy>>,
) -> Result<(), ConfigError> {
    let Some(policies) = policies else {
        return Ok(());
    };
    for (name, policy) in policies {
        if !valid_scope_label(name) {
            return Err(ConfigError::InvalidRoute {
                service: service.to_string(),
                reason: format!(
                    "rate_limits key '{}' is invalid (expected [a-z0-9:._-]+, max {})",
                    name, MAX_SCOPE_LEN
                ),
            });
        }
        if policy.requests == 0 {
            return Err(ConfigError::InvalidRoute {
                service: service.to_string(),
                reason: format!("rate_limits '{}' requests must be > 0", name),
            });
        }
        if policy.window_seconds == 0 {
            return Err(ConfigError::InvalidRoute {
                service: service.to_string(),
                reason: format!("rate_limits '{}' window_seconds must be > 0", name),
            });
        }
    }
    Ok(())
}

/// Shared policy names must agree on requests, window_seconds, and `shared`
/// across every service in `services` (full desired state).
pub fn validate_shared_rate_limits(
    services: &HashMap<String, ServiceConfig>,
) -> Result<(), ConfigError> {
    #[derive(Clone, Copy, PartialEq, Eq)]
    struct Sig {
        requests: u64,
        window_seconds: u64,
        shared: bool,
    }

    let mut by_name: HashMap<&str, Vec<(&str, Sig)>> = HashMap::new();
    for (svc_name, svc) in services {
        let Some(policies) = svc.rate_limits.as_ref() else {
            continue;
        };
        for (name, policy) in policies {
            by_name.entry(name.as_str()).or_default().push((
                svc_name.as_str(),
                Sig {
                    requests: policy.requests,
                    window_seconds: policy.window_seconds,
                    shared: policy.shared,
                },
            ));
        }
    }

    for (name, entries) in by_name {
        let any_shared = entries.iter().any(|(_, s)| s.shared);
        let any_local = entries.iter().any(|(_, s)| !s.shared);
        if any_shared && any_local {
            return Err(ConfigError::InvalidRoute {
                service: String::new(),
                reason: format!(
                    "rate_limits '{}' is shared on some services and local on others",
                    name
                ),
            });
        }
        if !any_shared {
            continue;
        }
        let first = entries[0].1;
        if entries.iter().any(|(_, s)| *s != first) {
            return Err(ConfigError::InvalidRoute {
                service: String::new(),
                reason: format!(
                    "shared rate_limits '{}' must use the same requests, window_seconds, and shared",
                    name
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

#[allow(dead_code)] // used by gateway; kept on both route_config copies
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
