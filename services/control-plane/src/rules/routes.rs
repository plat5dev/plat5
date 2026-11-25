//! Route-level validation: scopes, paths, templates, permissions, resources,
//! rate limits, shape collisions, and reserved gateway paths.

use std::collections::BTreeMap;

use crate::config::{Config, Keyword, RateLimit, Required, Resource, PUBLIC_SCOPE, USER_SCOPE};
use crate::expand::{self, At, ExpandedRoute, Spec};
use crate::schema::FieldError;

use super::path::{check_path, check_template, shape, Scope, MAX_PATH_LEN, MAX_PATH_PARAMS};

/// Rule 3 and 5 for one route: its scope, path, placeholders, permissions,
/// `resource`, rate limits, `audit`, and `allow_deleted`.
/// Returns true when its path can be matched.
pub(super) fn check_spec(config: &Config, spec: &Spec, errs: &mut Vec<FieldError>) -> bool {
    let scope = if config.kinds.contains_key(&spec.scope) {
        Scope::Kind(config.chain(&spec.scope))
    } else if spec.scope == USER_SCOPE {
        Scope::User
    } else if spec.scope == PUBLIC_SCOPE {
        Scope::Public
    } else {
        errs.push(FieldError::new(
            format!("services.{}.scopes.{}", spec.service, spec.scope),
            format!("{:?} is not a kind, user or public", spec.scope),
        ));
        return false;
    };
    let mut ok = true;
    let err = |errs: &mut Vec<FieldError>, at: &str, msg: String| {
        errs.push(FieldError::new(at, msg));
    };

    let params = match check_path(&spec.path) {
        Ok(params) => params,
        Err(msg) => {
            err(errs, &spec.path_at, msg);
            return false;
        }
    };
    if let Some(kind) = params.iter().find(|p| config.kinds.contains_key(*p)) {
        err(
            errs,
            &spec.path_at,
            format!("{{{kind}}} names a kind: a route's own params can't"),
        );
        ok = false;
    }
    let full = expand::public_path(config, spec);
    if full.len() > MAX_PATH_LEN {
        err(
            errs,
            &spec.path_at,
            format!("the public path {full} is longer than {MAX_PATH_LEN} bytes"),
        );
        ok = false;
    }
    if expand::params_of(&full).len() > MAX_PATH_PARAMS {
        err(
            errs,
            &spec.path_at,
            format!("the public path {full} has more than {MAX_PATH_PARAMS} params"),
        );
        ok = false;
    }
    if let Scope::Kind(_) = scope {
        for (child, kind) in config.children(&spec.scope) {
            let collection = kind.collection();
            if spec.path.starts_with(&format!("{collection}/")) {
                err(
                    errs,
                    &spec.path_at,
                    format!(
                        "under the {child} collection {collection}: the {child} with that slug would be shadowed. Only {collection} itself can be a route here"
                    ),
                );
                ok = false;
            }
        }
    }

    if let Some(p) = &spec.prefix {
        check_template(config, &spec.scope, &scope, &p.value, None, &p.at, errs);
    }
    if let Some(u) = &spec.upstream {
        check_template(
            config,
            &spec.scope,
            &scope,
            &u.value,
            Some(&params),
            &u.at,
            errs,
        );
    }

    // Validate shadowed route-level rate_limit / resource declarations too.
    if let Some(rl) = &spec.route_rate_limit {
        check_rate_limit(config, &spec.scope, &scope, rl, errs);
    }
    if let Some(res) = &spec.route_resource {
        check_resource(config, &spec.scope, &scope, &params, res, false, errs);
    }

    for v in &spec.verbs {
        match (&scope, &v.permissions) {
            (Scope::Kind(_), None) => err(
                errs,
                &v.missing_at,
                format!(
                    "required for {} in a {} scope: a permission, list of permissions, or any_member",
                    v.method, spec.scope
                ),
            ),
            (Scope::User | Scope::Public, Some(r)) => err(
                errs,
                &r.at,
                format!("a {} route takes no permissions", spec.scope),
            ),
            _ => {}
        }
        if let (Scope::Public, Some(a)) = (&scope, &v.audit) {
            err(errs, &a.at, "a public route has no audit log".into());
        }
        let is_any_member = matches!(
            v.permissions.as_ref().map(|r| &r.value),
            Some(Required::Keyword(Keyword::AnyMember))
        );
        if let Some(res) = &v.resource {
            check_resource(
                config,
                &spec.scope,
                &scope,
                &params,
                res,
                is_any_member,
                errs,
            );
        }
        if let Some(rl) = &v.rate_limit {
            check_rate_limit(config, &spec.scope, &scope, rl, errs);
        }
    }
    if let (Scope::User | Scope::Public, Some(a)) = (&scope, &spec.allow_deleted) {
        err(
            errs,
            &a.at,
            format!("only in a kind scope; this is {}", spec.scope),
        );
    }
    ok
}

fn check_resource(
    config: &Config,
    scope_name: &str,
    scope: &Scope,
    params: &[String],
    res: &At<Resource>,
    is_any_member: bool,
    errs: &mut Vec<FieldError>,
) {
    match scope {
        Scope::User | Scope::Public => errs.push(FieldError::new(
            &res.at,
            format!("only in a kind scope; this is {scope_name}"),
        )),
        Scope::Kind(_) => {
            if config.authorizer.is_none() {
                errs.push(FieldError::new(
                    &res.at,
                    "resource requires top-level authorizer to be configured",
                ));
            }
            if is_any_member {
                errs.push(FieldError::new(
                    &res.at,
                    "resource cannot be set when permissions is any_member",
                ));
            }
            check_template(
                config,
                scope_name,
                scope,
                &res.value.id,
                Some(params),
                &format!("{}.id", res.at),
                errs,
            );
        }
    }
}

fn check_rate_limit(
    config: &Config,
    scope_name: &str,
    scope: &Scope,
    rl: &At<RateLimit>,
    errs: &mut Vec<FieldError>,
) {
    let RateLimit::Policies(names) = &rl.value else {
        return;
    };
    for name in names {
        let Some(policy) = config.rate_limits.get(name) else {
            errs.push(FieldError::new(
                &rl.at,
                format!("{name:?} is not a policy in rate_limits"),
            ));
            continue;
        };
        let Some(count_at) = &policy.count_at else {
            continue;
        };
        match scope {
            Scope::Kind(chain) if chain.contains(count_at) => {}
            Scope::Kind(_) => errs.push(FieldError::new(
                &rl.at,
                format!("{name:?} counts at {count_at}, which isn't {scope_name} or an ancestor"),
            )),
            _ => errs.push(FieldError::new(
                &rl.at,
                format!(
                    "{name:?} counts at {count_at}: count_at is only for routes in a kind scope"
                ),
            )),
        }
    }
}

/// Rule 5: no two routes, in one service or across services, have the same
/// method and shape.
pub(super) fn check_shapes(routes: &[ExpandedRoute], errs: &mut Vec<FieldError>) {
    let mut first: BTreeMap<(String, String), &ExpandedRoute> = BTreeMap::new();
    for route in routes {
        let key = (route.method.clone(), shape(&route.path));
        match first.get(&key) {
            None => {
                first.insert(key, route);
            }
            Some(other) => errs.push(FieldError::new(
                route.at.clone(),
                format!(
                    "{} {} has the same method and shape as {} {} at {}",
                    route.method, route.path, other.method, other.path, other.at
                ),
            )),
        }
    }
}

fn matches_reserved(candidate: &str, reserved: &str) -> bool {
    let want: Vec<&str> = reserved.split('/').collect();
    let segments: Vec<&str> = candidate.split('/').collect();
    segments.len() == want.len()
        && segments
            .iter()
            .zip(&want)
            .all(|(have, w)| have.starts_with('{') || have == w)
}

/// Rule 4, for the gateway's own path: no route, in any method, matches
/// `/access/routes` (including host-relative paths under a root with `host`).
pub(super) fn check_route_checks(
    config: &Config,
    routes: &[ExpandedRoute],
    errs: &mut Vec<FieldError>,
) {
    let path = crate::config::ROUTE_CHECKS_PATH;
    for route in routes {
        if matches_reserved(&route.path, path) {
            errs.push(FieldError::new(
                route.at.clone(),
                format!(
                    "{} {} matches {path}, which is reserved for gateway route checks",
                    route.method, route.path
                ),
            ));
            continue;
        }
        // When the root kind uses subdomain host routing, the gateway strips the
        // root path prefix and matches the remainder against `/access/routes` first.
        if config.kinds.contains_key(&route.scope) {
            let chain = config.chain(&route.scope);
            if let Some(root_name) = chain.first() {
                if let Some(root_kind) = config.kinds.get(root_name) {
                    if root_kind.host.is_some() {
                        if let Some(rel) = route.path.strip_prefix(&root_kind.path) {
                            if !rel.is_empty() && matches_reserved(rel, path) {
                                errs.push(FieldError::new(
                                    route.at.clone(),
                                    format!(
                                        "{} {} matches {path} under host routing for {root_name}, which is reserved for gateway route checks",
                                        route.method, route.path
                                    ),
                                ));
                            }
                        }
                    }
                }
            }
        }
    }
}
