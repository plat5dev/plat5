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
