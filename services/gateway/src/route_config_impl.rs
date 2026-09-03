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

            validate_rate_limit_policies(name, service.rate_limits.as_ref())?;

            if let Some(ref public) = service.public {
                validate_scope_routes(name, "public", public, None, service.rate_limits.as_ref())?;
            }
            if let Some(ref user) = service.user {
                validate_scope_routes(name, "user", user, None, service.rate_limits.as_ref())?;
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
                validate_scope_routes(
                    name,
                    "organization",
                    org,
                    Some(param),
                    service.rate_limits.as_ref(),
                )?;
            }
        }
        validate_shared_rate_limits(&self.services)?;
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
