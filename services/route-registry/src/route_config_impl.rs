impl Config {
    pub fn validate(&self) -> Result<(), ConfigError> {
        for (name, service) in &self.services {
            validate_service_url(name, &service.url)?;
            if service.public.is_none()
                && service.user.is_none()
                && service.organization.is_none()
                && service.member.is_none()
            {
                return Err(ConfigError::InvalidRoute {
                    service: name.clone(),
                    reason: "service has no routes (public, user, organization, or member)"
                        .to_string(),
                });
            }
            validate_rate_limit_policies(name, service.rate_limits.as_ref())?;
            if let Some(ref public) = service.public {
                validate_scope_routes(name, "public", public, service.rate_limits.as_ref())?;
            }
            if let Some(ref user) = service.user {
                validate_scope_routes(name, "user", user, service.rate_limits.as_ref())?;
            }
            if let Some(ref org) = service.organization {
                validate_scope_routes(name, "organization", org, service.rate_limits.as_ref())?;
            }
            if let Some(ref member) = service.member {
                validate_scope_routes(name, "member", member, service.rate_limits.as_ref())?;
            }
        }
        validate_shared_rate_limits(&self.services)?;
        Ok(())
    }
}

impl ServiceConfig {
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
        if let Some(ref mut member) = self.member {
            expand_scope(member)?;
        }
        reject_duplicate_path_methods("", self)?;
        Ok(())
    }

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
                        upstream: route.upstream.clone(),
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

fn service_method_paths(svc: &ServiceConfig) -> impl Iterator<Item = (String, String)> + '_ {
    [
        svc.public.as_ref(),
        svc.user.as_ref(),
        svc.organization.as_ref(),
        svc.member.as_ref(),
    ]
    .into_iter()
    .flatten()
    .flat_map(|scope| scope.routes.iter())
    .flat_map(|r| r.methods.iter().map(|m| (m.clone(), r.path.clone())))
}

/// Method+path pairs in `incoming` already owned by a different service in
/// `existing` (or claimed by another incoming service). Services being applied
/// replace their own current routes, so those never conflict.
/// Returns sorted `"METHOD /path (owned by svc)"` lines.
pub fn find_route_conflicts(
    existing: &HashMap<String, ServiceConfig>,
    incoming: &[(String, ServiceConfig)],
) -> Vec<String> {
    let mut owners: HashMap<(String, String), &str> = HashMap::new();
    for (name, svc) in existing {
        if incoming.iter().any(|(n, _)| n == name) {
            continue;
        }
        for key in service_method_paths(svc) {
            owners.insert(key, name);
        }
    }
    let mut out = HashSet::new();
    for (name, svc) in incoming {
        for key in service_method_paths(svc) {
            match owners.get(&key) {
                Some(owner) if *owner != name => {
                    out.insert(format!("{} {} (owned by '{}')", key.0, key.1, owner));
                }
                Some(_) => {}
                None => {
                    owners.insert(key, name);
                }
            }
        }
    }
    let mut out: Vec<String> = out.into_iter().collect();
    out.sort();
    out
}

const URL_FORM: &str = "expected http://host:port, e.g. http://my-service:3000";

/// Service `url` must be exactly `http://host:port`: explicit port, no path or query.
fn validate_service_url(service: &str, url: &str) -> Result<(), ConfigError> {
    let bad = |reason: String| ConfigError::InvalidRoute {
        service: service.to_string(),
        reason,
    };
    if url.trim().is_empty() {
        return Err(bad(
            "service `url` is missing: add the service under `upstreams:` in plat5.yml, \
             or set `url` in the routes file"
                .to_string(),
        ));
    }
    if url
        .get(..8)
        .is_some_and(|p| p.eq_ignore_ascii_case("https://"))
    {
        return Err(bad(format!(
            "service url '{url}': TLS (https) upstreams aren't supported yet ({URL_FORM})"
        )));
    }
    let Some(rest) = url.strip_prefix("http://") else {
        return Err(bad(format!(
            "service url '{url}' must start with http:// ({URL_FORM})"
        )));
    };
    if rest.contains(['/', '?', '#']) {
        return Err(bad(format!(
            "service url '{url}' must not have a path or query ({URL_FORM})"
        )));
    }
    let port_ok = rest
        .rsplit_once(':')
        .map(|(host, port)| {
            !host.is_empty()
                && !host.contains(['@', ' '])
                && !port.is_empty()
                && port.parse::<u16>().is_ok_and(|p| p != 0)
        })
        .unwrap_or(false);
    if !port_ok {
        return Err(bad(format!(
            "service url '{url}' needs a host and an explicit port ({URL_FORM})"
        )));
    }
    Ok(())
}

fn reject_duplicate_path_methods(service: &str, svc: &ServiceConfig) -> Result<(), ConfigError> {
    let mut seen: HashSet<(String, String)> = HashSet::new();
    for scope in [
        svc.public.as_ref(),
        svc.user.as_ref(),
        svc.organization.as_ref(),
        svc.member.as_ref(),
    ]
    .into_iter()
    .flatten()
    {
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
