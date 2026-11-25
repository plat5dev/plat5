//! Route-level validation: scopes, paths, templates, permissions, authorizers,
//! rate limits, shape collisions, and reserved gateway paths.

use std::collections::BTreeMap;

use crate::config::{Config, Keyword, RateLimit, Required, PUBLIC_SCOPE, USER_SCOPE};
use crate::expand::{self, ExpandedRoute, Spec};
use crate::schema::FieldError;

use super::path::{check_path, check_template, shape, Scope, MAX_PATH_LEN, MAX_PATH_PARAMS};

/// Rule 4 and 6 for one route: its scope, path, placeholders, permissions,
/// rate limits, `audit`, `allow_deleted`, `allow_public`, and `authorizer`.
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

    let any_allow_public = spec
        .verbs
        .iter()
        .any(|v| v.allow_public.as_ref().is_some_and(|a| a.value));
    if let Some(p) = &spec.prefix {
        check_template(
            config,
            &spec.scope,
            &scope,
            &p.value,
            None,
            any_allow_public,
            &p.at,
            errs,
        );
    }
    if let Some(u) = &spec.upstream {
        check_template(
            config,
            &spec.scope,
            &scope,
            &u.value,
            Some(&params),
            any_allow_public,
            &u.at,
            errs,
        );
    }

    for v in &spec.verbs {
        match (&scope, &v.required) {
            (Scope::Kind(_), None) => err(
                errs,
                &v.missing_at,
                format!(
                    "required for {} in a {} scope: a list of permissions, or any_member",
                    v.method, spec.scope
                ),
            ),
            (Scope::User | Scope::Public, Some(r)) => err(
                errs,
                &r.at,
                format!("a {} route takes no required_permissions", spec.scope),
            ),
            _ => {}
        }
        if let (Scope::Public, Some(a)) = (&scope, &v.audit) {
            err(errs, &a.at, "a public route has no audit log".into());
        }
        let verb_allow_public = v.allow_public.as_ref().is_some_and(|a| a.value);
        if let Some(ap) = &v.allow_public {
            match &scope {
                Scope::User | Scope::Public => err(
                    errs,
                    &ap.at,
                    format!("only in a kind scope; this is {}", spec.scope),
                ),
                Scope::Kind(chain) if ap.value => {
                    if !chain.iter().any(|k| config.kinds[k].allow_public) {
                        err(
                            errs,
                            &ap.at,
                            format!(
                                "allow_public requires allow_public: true on {} or an ancestor kind",
                                spec.scope
                            ),
                        );
                    }
                    if matches!(
                        v.required.as_ref().map(|r| &r.value),
                        Some(Required::Keyword(Keyword::AnyMember))
                    ) {
                        err(
                            errs,
                            &ap.at,
                            "allow_public routes must require explicit permissions, not any_member"
                                .into(),
                        );
                    }
                }
                Scope::Kind(_) => {}
            }
        }
        if let Some(authz) = &v.authorizer {
            match &scope {
                Scope::User | Scope::Public => err(
                    errs,
                    &authz.at,
                    format!("only in a kind scope; this is {}", spec.scope),
                ),
                Scope::Kind(_) => {
                    if !config.authorizers.contains_key(&authz.value.name) {
                        err(
                            errs,
                            &authz.at,
                            format!("{:?} is not an authorizer in authorizers", authz.value.name),
                        );
                    }
                    if matches!(
                        v.required.as_ref().map(|r| &r.value),
                        Some(Required::Keyword(Keyword::AnyMember))
                    ) && authz.value.action.is_none()
                    {
                        err(
                            errs,
                            &authz.at,
                            "authorizer.action is required when required_permissions is any_member"
                                .into(),
                        );
                    }
                    if let Some(res) = &authz.value.resource {
                        check_template(
                            config,
                            &spec.scope,
                            &scope,
                            &res.id,
                            Some(&params),
                            verb_allow_public,
                            &format!("{}.resource.id", authz.at),
                            errs,
                        );
                    }
                }
            }
        }
        let (names, names_at): (Vec<String>, String) = match &v.rate_limit {
            Some(r) => match &r.value {
                RateLimit::Policies(p) => (p.clone(), r.at.clone()),
                RateLimit::Off(_) => (Vec::new(), r.at.clone()),
            },
            // Omitted is `defaults.rate_limit`, checked once for every route.
            None => (Vec::new(), String::new()),
        };
        for name in names {
            let Some(policy) = config.rate_limits.get(&name) else {
                err(
                    errs,
                    &names_at,
                    format!("{name:?} is not a policy in rate_limits"),
                );
                continue;
            };
            let Some(count_at) = &policy.count_at else {
                continue;
            };
            match &scope {
                Scope::Kind(chain) if chain.contains(count_at) => {}
                Scope::Kind(_) => err(
                    errs,
                    &names_at,
                    format!(
                        "{name:?} counts at {count_at}, which isn't {} or an ancestor",
                        spec.scope
                    ),
                ),
                _ => err(
                    errs,
                    &names_at,
                    format!(
                        "{name:?} counts at {count_at}: count_at is only for routes in a kind scope"
                    ),
                ),
            }
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

/// Rule 4, for the gateway's own path: no route, in any method, matches
/// `/access/routes` (docs/reference/gateway.md#route-checks). Each `{param}`
/// matches any segment, a literal only itself.
pub(super) fn check_route_checks(routes: &[ExpandedRoute], errs: &mut Vec<FieldError>) {
    let path = crate::config::ROUTE_CHECKS_PATH;
    let want: Vec<&str> = path.split('/').collect();
    for route in routes {
        let segments: Vec<&str> = route.path.split('/').collect();
        let matches = segments.len() == want.len()
            && segments
                .iter()
                .zip(&want)
                .all(|(have, w)| have.starts_with('{') || have == w);
        if matches {
            errs.push(FieldError::new(
                route.at.clone(),
                format!(
                    "{} {} matches {path}, which is reserved for gateway route checks",
                    route.method, route.path
                ),
            ));
        }
    }
}
