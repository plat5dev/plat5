//! What the schema can't say: the rules that tie the documents together
//! (docs/concepts/config.md#validation-at-apply) and the route rules
//! (docs/reference/config/services.md#validation), over every expanded route,
//! identity's and audit's included. The control plane is the one validator: the
//! gateway and identity trust what passes.

use std::collections::{BTreeMap, BTreeSet};

use crate::config::{Config, Keyword, RateLimit, Required, PUBLIC_SCOPE, USER_SCOPE};
use crate::expand::{self, ExpandedRoute, Spec};
use crate::schema::FieldError;

const MAX_PATH_LEN: usize = 1024;
const MAX_PATH_PARAMS: usize = 32;
const MAX_PARAM_NAME_LEN: usize = 64;

/// Every rule, over a config that already passed the schema. Each error names
/// a field path.
pub fn check(config: &Config) -> Result<(), Vec<FieldError>> {
    let mut errs = Vec::new();
    check_kinds(config, &mut errs);
    // The rest reads the tree; with a broken tree its errors are noise.
    if errs.is_empty() {
        check_policies(config, &mut errs);
        check_service_authorizers(config, &mut errs);
        let mut valid = Vec::new();
        for spec in &expand::specs(config) {
            if check_spec(config, spec, &mut errs) {
                valid.extend(expand::expand_spec(config, spec));
            }
        }
        check_shapes(&valid, &mut errs);
        check_route_checks(&valid, &mut errs);
        check_roles(config, &mut errs);
        check_permissions(config, &mut errs);
    }
    if errs.is_empty() {
        return Ok(());
    }
    let mut seen = BTreeSet::new();
    errs.retain(|e| seen.insert((e.path.clone(), e.message.clone())));
    Err(errs)
}

/// Rule 1: every parent is a kind, no cycles, each kind's path param is
/// its own name, and optional `host` templates sit only on root kinds with
/// distinct domain suffixes.
fn check_kinds(config: &Config, errs: &mut Vec<FieldError>) {
    let mut host_suffixes: BTreeMap<String, String> = BTreeMap::new();
    for (name, kind) in &config.kinds {
        if let Some(parent) = &kind.parent {
            if !config.kinds.contains_key(parent) {
                errs.push(FieldError::new(
                    format!("kinds.{name}.parent"),
                    format!("{parent:?} is not a kind"),
                ));
            }
        }
        if kind.param() != name {
            errs.push(FieldError::new(
                format!("kinds.{name}.path"),
                format!(
                    "the param is the kind's name: {}/{{{name}}}",
                    kind.collection()
                ),
            ));
        }
        if let Some((param, suffix)) = kind.host_parts() {
            if kind.parent.is_some() {
                errs.push(FieldError::new(
                    format!("kinds.{name}.host"),
                    "host is only allowed on root kinds",
                ));
            } else if param != name {
                errs.push(FieldError::new(
                    format!("kinds.{name}.host"),
                    format!("the host param must be the kind's name: {{{name}}}{suffix}"),
                ));
            } else if let Some(other) = host_suffixes.insert(suffix.to_string(), name.clone()) {
                errs.push(FieldError::new(
                    format!("kinds.{name}.host"),
                    format!("host suffix {suffix:?} is already used by kind {other:?}"),
                ));
            }
        }
        if let Some(authz) = &kind.authorizer {
            if !config.authorizers.contains_key(&authz.name) {
                errs.push(FieldError::new(
                    format!("kinds.{name}.authorizer"),
                    format!("{:?} is not an authorizer in authorizers", authz.name),
                ));
            }
        }
    }
    let mut reported = BTreeSet::new();
    for name in config.kinds.keys() {
        let mut cycle = vec![name.as_str()];
        let mut at = name.as_str();
        while let Some(parent) = config.kinds.get(at).and_then(|k| k.parent.as_deref()) {
            if parent == name {
                cycle.push(parent);
                let first = cycle[..cycle.len() - 1].iter().min().expect("not empty");
                if reported.insert(first.to_string()) {
                    errs.push(FieldError::new(
                        format!("kinds.{first}.parent"),
                        format!("parents form a cycle: {}", cycle.join(" → ")),
                    ));
                }
                break;
            }
            if cycle.contains(&parent) || cycle.len() > config.kinds.len() {
                break;
            }
            cycle.push(parent);
            at = parent;
        }
    }
}

fn check_service_authorizers(config: &Config, errs: &mut Vec<FieldError>) {
    for (name, service) in &config.services {
        if let Some(resolved) = service.authorizer.as_ref().and_then(|a| a.resolve()) {
            if !config.authorizers.contains_key(&resolved.name) {
                errs.push(FieldError::new(
                    format!("services.{name}.authorizer"),
                    format!("{:?} is not an authorizer in authorizers", resolved.name),
                ));
            }
        }
    }
}

/// Rule 5, for the policies: a `count_at` is a kind, and each policy
/// `defaults.rate_limit` names is in `rate_limits` and has no `count_at`.
fn check_policies(config: &Config, errs: &mut Vec<FieldError>) {
    for (name, policy) in &config.rate_limits {
        if let Some(kind) = &policy.count_at {
            if !config.kinds.contains_key(kind) {
                errs.push(FieldError::new(
                    format!("rate_limits.{name}.count_at"),
                    format!("{kind:?} is not a kind"),
                ));
            }
        }
    }
    for (i, name) in config.defaults.rate_limit.iter().enumerate() {
        let at = format!("defaults.rate_limit[{i}]");
        match config.rate_limits.get(name) {
            None => errs.push(FieldError::new(
                at,
                format!("{name:?} is not a policy in rate_limits"),
            )),
            Some(p) if p.count_at.is_some() => errs.push(FieldError::new(
                at,
                format!("{name:?} counts at a kind: a default applies to user and public routes too, which have no node to count at"),
            )),
            Some(_) => {}
        }
    }
}

/// Rule 2: roles bind at kinds and include roles, and `creator_role` binds
/// everywhere. A role may grant any permission
/// (docs/reference/config/roles.md).
fn check_roles(config: &Config, errs: &mut Vec<FieldError>) {
    for (slug, role) in &config.roles {
        for (i, kind) in role.bindable_at.iter().enumerate() {
            if !config.kinds.contains_key(kind) {
                errs.push(FieldError::new(
                    format!("roles.{slug}.bindable_at[{i}]"),
                    format!("{kind:?} is not a kind"),
                ));
            }
        }
        for (i, included) in role.includes.iter().enumerate() {
            if !config.roles.contains_key(included) {
                errs.push(FieldError::new(
                    format!("roles.{slug}.includes[{i}]"),
                    format!("{included:?} is not a role"),
                ));
            }
        }
    }
    if let Some(cycle) = include_cycle(config) {
        errs.push(FieldError::new(
            format!("roles.{}.includes", cycle[0]),
            format!("includes form a cycle: {}", cycle.join(" → ")),
        ));
    }
    match config.roles.get(&config.creator_role) {
        None => errs.push(FieldError::new(
            "creator_role",
            format!("{:?} is not a role", config.creator_role),
        )),
        Some(role) => {
            let missing: Vec<&str> = config
                .kinds
                .keys()
                .filter(|k| !role.bindable_at.contains(k))
                .map(String::as_str)
                .collect();
            if !missing.is_empty() {
                errs.push(FieldError::new(
                    "creator_role",
                    format!(
                        "{:?} must be bindable at every kind; it isn't at {}",
                        config.creator_role,
                        missing.join(", ")
                    ),
                ));
            }
        }
    }
}

fn check_permissions(config: &Config, errs: &mut Vec<FieldError>) {
    for (perm, meta) in &config.permissions {
        for (i, kind) in meta.bindable_at.iter().enumerate() {
            if !config.kinds.contains_key(kind) {
                errs.push(FieldError::new(
                    format!("permissions.{perm}.bindable_at[{i}]"),
                    format!("{kind:?} is not a kind"),
                ));
            }
        }
    }
}

fn include_cycle(config: &Config) -> Option<Vec<String>> {
    fn visit(
        config: &Config,
        role: &str,
        stack: &mut Vec<String>,
        done: &mut BTreeSet<String>,
    ) -> Option<Vec<String>> {
        if let Some(at) = stack.iter().position(|r| r == role) {
            let mut cycle = stack[at..].to_vec();
            cycle.push(role.to_string());
            return Some(cycle);
        }
        if !done.insert(role.to_string()) {
            return None;
        }
        stack.push(role.to_string());
        for included in config.roles.get(role).map_or(&[][..], |r| &r.includes) {
            if let Some(c) = visit(config, included, stack, done) {
                return Some(c);
            }
        }
        stack.pop();
        None
    }
    let mut done = BTreeSet::new();
    config
        .roles
        .keys()
        .find_map(|r| visit(config, r, &mut Vec::new(), &mut done))
}

/// What a route's scope gives it.
enum Scope {
    /// A kind, with its chain of kinds, root first.
    Kind(Vec<String>),
    User,
    Public,
}

/// Rule 4 and 6 for one route: its scope, path, placeholders, permissions,
/// rate limits, `audit` and `allow_deleted`. True when its path can be matched.
fn check_spec(config: &Config, spec: &Spec, errs: &mut Vec<FieldError>) -> bool {
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

/// The placeholders of an upstream template: `{<kind>.id}` for a kind in the
/// chain, `{member.id}` in a kind scope, `{user.id}` in `user`, and in a
/// route's `upstream` (`own` is its params) a bare `{param}` of the route's.
fn check_template(
    config: &Config,
    scope_name: &str,
    scope: &Scope,
    template: &str,
    own: Option<&[String]>,
    at: &str,
    errs: &mut Vec<FieldError>,
) {
    let mut rest = template;
    loop {
        let Some(start) = rest.find('{') else {
            if rest.contains('}') {
                errs.push(FieldError::new(at, "unopened }"));
            }
            return;
        };
        if rest[..start].contains('}') {
            errs.push(FieldError::new(at, "unopened }"));
            return;
        }
        let after = &rest[start + 1..];
        let Some(end) = after.find('}') else {
            errs.push(FieldError::new(at, "unclosed {"));
            return;
        };
        let body = &after[..end];
        rest = &after[end + 1..];
        let problem = match body.split_once('.') {
            Some((_, field)) if field != "id" => Some(format!(
                "{{{body}}}: only .id exists, so slugs never reach a service"
            )),
            Some(("member", _)) => match scope {
                Scope::Kind(_) => None,
                _ => Some(format!(
                    "{{member.id}} is only in a kind scope; this is {scope_name}"
                )),
            },
            Some(("user", _)) => match scope {
                Scope::User => None,
                _ => Some(format!(
                    "{{user.id}} is only in the user scope; this is {scope_name}"
                )),
            },
            Some((kind, _)) => match scope {
                Scope::Kind(chain) if chain.iter().any(|k| k == kind) => None,
                Scope::Kind(_) if config.kinds.contains_key(kind) => Some(format!(
                    "{{{body}}}: {kind} isn't in the {scope_name} chain"
                )),
                Scope::Kind(_) => Some(format!("{{{body}}}: {kind:?} is not a kind")),
                _ => Some(format!("{{{body}}}: a {scope_name} route has no node")),
            },
            None => match own {
                None => Some(format!(
                    "{{{body}}}: upstream_prefix takes only placeholders with a dot"
                )),
                Some(params) if params.iter().any(|p| p == body) => None,
                Some(_) => Some(format!("{{{body}}} isn't one of the route's params")),
            },
        };
        if let Some(p) = problem {
            errs.push(FieldError::new(at, p));
        }
    }
}

/// A route path's params, once its canonical form is checked: no trailing `/`
/// (but `/`), no empty segment, lowercase outside params, visible ASCII, each
/// param a whole segment named `[A-Za-z_][A-Za-z0-9_]*`, at most 64, once.
pub fn check_path(path: &str) -> Result<Vec<String>, String> {
    if !path.starts_with('/') {
        return Err("must start with /".into());
    }
    if path == "/" {
        return Ok(Vec::new());
    }
    if !path.bytes().all(|b| b.is_ascii_graphic()) {
        return Err("must be visible ASCII: no spaces; percent-encode anything else".into());
    }
    if path.ends_with('/') {
        return Err(format!(
            "must not end with / (use {})",
            path.trim_end_matches('/')
        ));
    }
    let mut params: Vec<String> = Vec::new();
    for segment in path[1..].split('/') {
        if segment.is_empty() {
            return Err("has an empty segment".into());
        }
        if !segment.contains(['{', '}']) {
            if segment.chars().any(|c| c.is_ascii_uppercase()) {
                return Err("must be lowercase outside {params}".into());
            }
            continue;
        }
        let Some(name) = segment
            .strip_prefix('{')
            .and_then(|s| s.strip_suffix('}'))
            .filter(|n| !n.contains(['{', '}']))
        else {
            return Err(format!(
                "{segment}: a param is a whole segment, such as /files/{{name}}"
            ));
        };
        if !valid_param_name(name) {
            return Err(format!(
                "param {{{name}}} must be 1-{MAX_PARAM_NAME_LEN} of letters, digits or _, not starting with a digit"
            ));
        }
        if params.iter().any(|p| p == name) {
            return Err(format!("param {{{name}}} appears twice"));
        }
        params.push(name.to_string());
    }
    Ok(params)
}

fn valid_param_name(name: &str) -> bool {
    let mut bytes = name.bytes();
    name.len() <= MAX_PARAM_NAME_LEN
        && bytes
            .next()
            .is_some_and(|b| b.is_ascii_alphabetic() || b == b'_')
        && bytes.all(|b| b.is_ascii_alphanumeric() || b == b'_')
}

/// A path's matching shape: each `{param}` is `{}`; literals stay.
fn shape(path: &str) -> String {
    let mut out = String::with_capacity(path.len());
    for segment in path.split('/').skip(1) {
        out.push('/');
        out.push_str(if segment.starts_with('{') {
            "{}"
        } else {
            segment
        });
    }
    out
}

/// Rule 5: no two routes, in one service or across services, have the same
/// method and shape.
fn check_shapes(routes: &[ExpandedRoute], errs: &mut Vec<FieldError>) {
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
fn check_route_checks(routes: &[ExpandedRoute], errs: &mut Vec<FieldError>) {
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

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::{json, Value};

    fn base() -> Value {
        json!({
            "kinds": {
                "organization": { "path": "/orgs/{organization}", "list_permission": "organization:read" },
                "project": { "parent": "organization", "path": "/projects/{project}", "list_permission": "project:read" }
            },
            "roles": {
                "admin": { "permissions": ["*"], "bindable_at": ["organization", "project"] },
                "member": { "permissions": ["organization:read"], "bindable_at": ["organization"] }
            },
            "creator_role": "admin",
            "defaults": { "rate_limit": ["standard"] },
            "rate_limits": {
                "standard": { "requests": 60, "window": "60s" },
                "per-project": { "requests": 100, "window": "1s", "count_at": "project" },
                "per-org": { "requests": 300, "window": "1s", "count_at": "organization" }
            }
        })
    }

    fn run(v: Value) -> Result<(), Vec<FieldError>> {
        crate::schema::check_config(&v).unwrap_or_else(|e| panic!("schema: {e:?}"));
        let config: Config = serde_json::from_value(v).unwrap();
        check(&config)
    }

    fn errors_at(v: Value, path: &str) -> Vec<FieldError> {
        match run(v) {
            Err(errs) => {
                assert!(errs.iter().any(|e| e.path == path), "want {path}: {errs:?}");
                errs
            }
            Ok(()) => panic!("want invalid at {path}, got ok"),
        }
    }

    /// The base with one e10s scope holding `routes`.
    fn with_routes(scope: &str, routes: Value) -> Value {
        let mut c = base();
        c["services"] = json!({ "e10s": {
            "url": "http://e10s:3000",
            "scopes": { scope: { "routes": routes } }
        } });
        c
    }

    fn route(scope: &str, route: Value) -> Value {
        with_routes(scope, json!([route]))
    }

    const AT: &str = "services.e10s.scopes";

    #[test]
    fn the_base_passes() {
        run(base()).unwrap();
        run(route(
            "project",
            json!({ "path": "/", "methods": ["GET"], "required_permissions": ["organization:read"] }),
        ))
        .unwrap();
    }

    #[test]
    fn kinds() {
        let mut c = base();
        c["kinds"]["project"]["parent"] = json!("folder");
        errors_at(c, "kinds.project.parent");
        let mut c = base();
        c["kinds"]["organization"]["parent"] = json!("project");
        errors_at(c, "kinds.organization.parent");
        let mut c = base();
        c["kinds"]["project"]["path"] = json!("/projects/{id}");
        errors_at(c, "kinds.project.path");

        // host template on root kind passes; child kind, wrong param, and duplicate suffix fail.
        let mut c = base();
        c["kinds"]["organization"]["host"] = json!("{organization}.api.example.com");
        run(c).unwrap();

        let mut c = base();
        c["kinds"]["project"]["host"] = json!("{project}.api.example.com");
        errors_at(c, "kinds.project.host");

        let mut c = base();
        c["kinds"]["organization"]["host"] = json!("{org}.api.example.com");
        errors_at(c, "kinds.organization.host");

        let mut c = base();
        c["kinds"]["organization"]["host"] = json!("{organization}.api.example.com");
        c["kinds"]["workspace"] = json!({
            "path": "/workspaces/{workspace}",
            "host": "{workspace}.api.example.com"
        });
        c["roles"]["admin"]["bindable_at"] = json!(["organization", "project", "workspace"]);
        errors_at(c, "kinds.workspace.host");

        // rate_limits tiers pass schema and rules.
        let mut c = base();
        c["rate_limits"]["per-org"]["tiers"] = json!({
            "pro": { "requests": 1000, "window": "1s" },
            "enterprise": { "requests": 5000, "window": "1s" }
        });
        run(c).unwrap();

        // Tier 2: custom_roles, permissions catalog, and allow_public kinds/routes.
        let mut c = base();
        c["custom_roles"] = json!(true);
        c["permissions"] = json!({
            "reports:export": { "description": "Export reports", "bindable_at": ["project"] },
            "internal:admin": { "custom_role_assignable": false }
        });
        run(c).unwrap();

        let mut c = base();
        c["permissions"] = json!({
            "reports:export": { "bindable_at": ["ghost"] }
        });
        errors_at(c, "permissions.reports:export.bindable_at[0]");

        // allow_public on a route requires allow_public on the kind or an ancestor, and explicit permissions.
        let c = route(
            "project",
            json!({ "path": "/public-items", "methods": ["GET"], "required_permissions": ["project:read"], "allow_public": true }),
        );
        errors_at(c, &format!("{AT}.project.routes[0].allow_public"));

        let mut c = route(
            "project",
            json!({ "path": "/public-items", "methods": ["GET"], "required_permissions": ["project:read"], "allow_public": true }),
        );
        c["kinds"]["project"]["allow_public"] = json!(true);
        run(c).unwrap();

        // Ancestor allow_public also enables descendant allow_public routes.
        let mut c = route(
            "project",
            json!({ "path": "/public-items", "methods": ["GET"], "required_permissions": ["project:read"], "allow_public": true }),
        );
        c["kinds"]["organization"]["allow_public"] = json!(true);
        run(c).unwrap();

        let mut c = route(
            "project",
            json!({ "path": "/public-items", "methods": ["GET"], "required_permissions": "any_member", "allow_public": true }),
        );
        c["kinds"]["project"]["allow_public"] = json!(true);
        errors_at(c, &format!("{AT}.project.routes[0].allow_public"));

        let c = route(
            "user",
            json!({ "path": "/user/a", "methods": ["GET"], "allow_public": true }),
        );
        errors_at(c, &format!("{AT}.user.routes[0].allow_public"));
    }

    #[test]
    fn authorizers() {
        let mut c = base();
        c["authorizers"] = json!({
            "topaz": {
                "url": "http://topaz:8383",
                "timeout": "3s",
                "cache_ttl": "5s",
                "negative_cache_ttl": "1s",
                "token_env": "TOPAZ_API_KEY"
            }
        });
        c["kinds"]["project"]["authorizer"] = json!({ "name": "topaz", "mode": "or" });
        c["services"] = json!({
            "docs": {
                "url": "http://docs:3000",
                "authorizer": "topaz",
                "scopes": {
                    "project": {
                        "upstream_prefix": "/projects/{project.id}",
                        "routes": [
                            {
                                "path": "/docs",
                                "methods": ["GET"],
                                "required_permissions": ["project:read"]
                            },
                            {
                                "path": "/docs/{doc_id}",
                                "methods": {
                                    "GET": {
                                        "required_permissions": ["project:read"],
                                        "authorizer": {
                                            "name": "topaz",
                                            "mode": "and",
                                            "resource": { "type": "document", "id": "{doc_id}" }
                                        }
                                    },
                                    "DELETE": {
                                        "required_permissions": "any_member",
                                        "authorizer": {
                                            "name": "topaz",
                                            "mode": "replace",
                                            "resource": { "type": "document", "id": "{project.id}:{doc_id}" },
                                            "action": "document:delete"
                                        }
                                    }
                                }
                            },
                            {
                                "path": "/summary",
                                "methods": ["GET"],
                                "required_permissions": ["project:read"],
                                "authorizer": false
                            }
                        ]
                    },
                    "public": {
                        "routes": [{ "path": "/health", "methods": ["GET"] }]
                    }
                }
            }
        });
        run(c.clone()).unwrap();

        // Unknown authorizer on kind, service, and route are refused.
        let mut bad = base();
        bad["kinds"]["project"]["authorizer"] = json!("missing");
        errors_at(bad, "kinds.project.authorizer");

        let mut bad = c.clone();
        bad["services"]["docs"]["authorizer"] = json!("missing");
        errors_at(bad, "services.docs.authorizer");

        // Explicit authorizer on user or public scope is refused.
        let mut bad = c.clone();
        bad["services"]["docs"]["scopes"]["public"]["routes"][0]["authorizer"] = json!("topaz");
        errors_at(bad, "services.docs.scopes.public.routes[0].authorizer");

        // any_member with authorizer requires an explicit action.
        let mut bad = c.clone();
        bad["services"]["docs"]["scopes"]["project"]["routes"][1]["methods"]["DELETE"]
            ["authorizer"] = json!({ "name": "topaz", "mode": "replace" });
        errors_at(
            bad,
            "services.docs.scopes.project.routes[1].methods.DELETE.authorizer",
        );

        // Unknown placeholder in resource.id is refused.
        let mut bad = c.clone();
        bad["services"]["docs"]["scopes"]["project"]["routes"][1]["methods"]["GET"]["authorizer"]
            ["resource"]["id"] = json!("{nope}");
        errors_at(
            bad,
            "services.docs.scopes.project.routes[1].methods.GET.authorizer.resource.id",
        );
    }

    #[test]
    fn roles() {
        let mut c = base();
        c["roles"]["member"]["bindable_at"] = json!(["team"]);
        errors_at(c, "roles.member.bindable_at[0]");
        let mut c = base();
        c["roles"]["member"]["includes"] = json!(["ghost"]);
        errors_at(c, "roles.member.includes[0]");
        let mut c = base();
        c["roles"]["a"] = json!({ "includes": ["b"], "bindable_at": ["organization"] });
        c["roles"]["b"] = json!({ "includes": ["a"], "bindable_at": ["organization"] });
        errors_at(c, "roles.a.includes");
        let mut c = base();
        c["creator_role"] = json!("member");
        errors_at(c, "creator_role");
        let mut c = base();
        c["creator_role"] = json!("owner");
        errors_at(c, "creator_role");
    }

    // A role may grant a permission no route requires: one checked only
    // through identity's evaluation API, or a typo, which grants nothing.
    #[test]
    fn a_role_may_grant_any_permission() {
        let mut c = base();
        c["roles"]["member"]["permissions"] = json!(["organization:read", "reports:export"]);
        run(c).unwrap();
        // And a route may require one no role grants.
        run(route(
            "project",
            json!({ "path": "/a", "methods": ["GET"], "required_permissions": ["nobody:has"] }),
        ))
        .unwrap();
    }

    #[test]
    fn paths() {
        let bad = [
            "/a/",
            "/a//b",
            "/A",
            "/files/{name}.json",
            "/x{a}",
            "/{a}/{a}",
            "/{9a}",
        ];
        for p in bad {
            errors_at(
                route("public", json!({ "path": p, "methods": ["GET"] })),
                &format!("{AT}.public.routes[0].path"),
            );
        }
        // A route's own params can't be kind names.
        errors_at(
            route(
                "public",
                json!({ "path": "/x/{project}", "methods": ["GET"] }),
            ),
            &format!("{AT}.public.routes[0].path"),
        );
    }

    #[test]
    fn a_child_collection() {
        let at = format!("{AT}.organization.routes[0].path");
        let r = |path: &str| {
            route(
                "organization",
                json!({ "path": path, "methods": ["GET"], "required_permissions": ["organization:read"] }),
            )
        };
        errors_at(r("/projects/archived"), &at);
        errors_at(r("/projects/{x}/y"), &at);
        // The collection itself, and a literal that only starts the same.
        run(r("/archived-projects")).unwrap();
        let mut c = r("/projects");
        c["services"]["e10s"]["scopes"]["organization"]["routes"][0]["methods"] = json!(["PUT"]);
        run(c).unwrap();
    }

    #[test]
    fn scopes_and_permissions() {
        let c = route("team", json!({ "path": "/a", "methods": ["GET"] }));
        errors_at(c, &format!("{AT}.team"));
        errors_at(
            route("project", json!({ "path": "/a", "methods": ["GET"] })),
            &format!("{AT}.project.routes[0].required_permissions"),
        );
        errors_at(
            route(
                "project",
                json!({ "path": "/a", "methods": { "GET": null, "POST": { "required_permissions": ["organization:read"] } } }),
            ),
            &format!("{AT}.project.routes[0].methods.GET.required_permissions"),
        );
        run(route(
            "project",
            json!({ "path": "/a", "required_permissions": "any_member", "methods": { "GET": null, "POST": { "required_permissions": ["organization:read"] } } }),
        ))
        .unwrap();
        errors_at(
            route(
                "user",
                json!({ "path": "/user/a", "methods": ["GET"], "required_permissions": ["organization:read"] }),
            ),
            &format!("{AT}.user.routes[0].required_permissions"),
        );
        errors_at(
            route(
                "public",
                json!({ "path": "/a", "methods": { "POST": { "audit": true } } }),
            ),
            &format!("{AT}.public.routes[0].methods.POST.audit"),
        );
        errors_at(
            route(
                "user",
                json!({ "path": "/user/a", "methods": ["GET"], "allow_deleted": true }),
            ),
            &format!("{AT}.user.routes[0].allow_deleted"),
        );
    }

    #[test]
    fn placeholders() {
        let ok = |scope: &str, prefix: Option<&str>, path: &str, upstream: Option<&str>| {
            let mut r = json!({ "path": path, "methods": ["GET"] });
            if scope != "user" && scope != "public" {
                r["required_permissions"] = json!("any_member");
            }
            if let Some(u) = upstream {
                r["upstream"] = json!(u);
            }
            let mut c = route(scope, r);
            if let Some(p) = prefix {
                c["services"]["e10s"]["scopes"][scope]["upstream_prefix"] = json!(p);
            }
            c
        };
        run(ok(
            "project",
            Some("/p/{project.id}/o/{organization.id}/m/{member.id}"),
            "/a/{x}",
            None,
        ))
        .unwrap();
        run(ok("project", None, "/a/{x}", Some("/b/{x}/{project.id}"))).unwrap();
        run(ok("user", Some("/users/{user.id}"), "/user/a", None)).unwrap();
        let at = |scope: &str, f: &str| format!("{AT}.{scope}.{f}");
        for (scope, prefix, upstream, field) in [
            (
                "organization",
                Some("/p/{project.id}"),
                None,
                "upstream_prefix",
            ),
            (
                "project",
                Some("/p/{project.slug}"),
                None,
                "upstream_prefix",
            ),
            ("project", Some("/p/{x}"), None, "upstream_prefix"),
            ("project", Some("/p/{user.id}"), None, "upstream_prefix"),
            ("project", None, Some("/p/{y}"), "routes[0].upstream"),
            ("project", None, Some("/p/{team.id}"), "routes[0].upstream"),
            ("user", None, Some("/p/{member.id}"), "routes[0].upstream"),
            ("public", None, Some("/p/{user.id}"), "routes[0].upstream"),
            ("public", None, Some("/p/{x"), "routes[0].upstream"),
        ] {
            let path = if scope == "user" {
                "/user/a/{x}"
            } else {
                "/a/{x}"
            };
            errors_at(ok(scope, prefix, path, upstream), &at(scope, field));
        }
    }

    #[test]
    fn rate_limits() {
        let r = |scope: &str, rl: Value| {
            let mut r = json!({ "path": "/a", "methods": ["GET"], "rate_limit": rl });
            if scope == "organization" || scope == "project" {
                r["required_permissions"] = json!("any_member");
            }
            route(scope, r)
        };
        run(r("project", json!(["per-project", "per-org"]))).unwrap();
        let at = |scope: &str| format!("{AT}.{scope}.routes[0].rate_limit");
        errors_at(r("project", json!(["ghost"])), &at("project"));
        errors_at(
            r("organization", json!(["per-project"])),
            &at("organization"),
        );
        errors_at(r("public", json!(["per-org"])), &at("public"));
        let mut c = base();
        c["rate_limits"]["per-team"] = json!({ "requests": 1, "window": "1s", "count_at": "team" });
        errors_at(c, "rate_limits.per-team.count_at");
        // defaults.rate_limit names policies, none with count_at.
        let mut c = base();
        c["defaults"]["rate_limit"] = json!(["standard", "ghost"]);
        errors_at(c, "defaults.rate_limit[1]");
        let mut c = base();
        c["defaults"]["rate_limit"] = json!(["per-org"]);
        errors_at(c, "defaults.rate_limit[0]");
        // No policy name is special: `default` is an ordinary one, and the
        // standard policy can count at a kind when no default names it.
        let mut c = base();
        c["rate_limits"]["default"] =
            json!({ "requests": 1, "window": "1s", "count_at": "organization" });
        run(c).unwrap();
        // Omitted is defaults.rate_limit, in every scope; `false` is none.
        let mut c = with_routes(
            "public",
            json!([
                { "path": "/a", "methods": ["GET"] },
                { "path": "/b", "methods": ["GET"], "rate_limit": false }
            ]),
        );
        c["rate_limits"]["burst"] = json!({ "requests": 5, "window": "1s" });
        c["defaults"]["rate_limit"] = json!(["standard", "burst"]);
        run(c.clone()).unwrap();
        let config: Config = serde_json::from_value(c).unwrap();
        let routes = &expand::expand(&config)["e10s"].routes;
        assert_eq!(routes[0].rate_limit, vec!["standard", "burst"]);
        assert!(routes[1].rate_limit.is_empty());
        // Rate limits and defaults are optional: omitted routes are unlimited.
        let mut c = with_routes("public", json!([{ "path": "/a", "methods": ["GET"] }]));
        c.as_object_mut().unwrap().remove("defaults");
        c.as_object_mut().unwrap().remove("rate_limits");
        run(c.clone()).unwrap();
        let config: Config = serde_json::from_value(c).unwrap();
        assert!(expand::expand(&config)["e10s"].routes[0]
            .rate_limit
            .is_empty());
    }

    #[test]
    fn same_shape_twice() {
        let c = with_routes(
            "public",
            json!([
                { "path": "/a/{x}", "methods": ["GET"] },
                { "path": "/a/{y}", "methods": ["GET", "POST"] }
            ]),
        );
        let errs = errors_at(c, &format!("{AT}.public.routes[1].path"));
        assert!(errs[0]
            .message
            .contains(&format!("{AT}.public.routes[0].path")));
        // Across services.
        let mut c = route(
            "organization",
            json!({ "path": "/", "methods": ["GET"], "required_permissions": ["organization:read"] }),
        );
        c["services"]["identity"] = identity();
        let errs = errors_at(c, "services.identity.scopes.organization.routes[0].path");
        assert!(
            errs[0]
                .message
                .contains(&format!("{AT}.organization.routes[0].path")),
            "{errs:?}"
        );
        // A literal and a param differ.
        run(with_routes(
            "public",
            json!([
                { "path": "/a/b", "methods": ["GET"] },
                { "path": "/a/{y}", "methods": ["GET"] }
            ]),
        ))
        .unwrap();
    }

    #[test]
    fn a_route_matching_the_route_checks_path() {
        let mut c = base();
        c["services"] = json!({ "identity": identity() });
        run(c).unwrap();

        let at = format!("{AT}.public.routes[0].path");
        let c = route(
            "public",
            json!({ "path": "/access/routes", "methods": ["GET"] }),
        );
        errors_at(c, &at);

        // A param matches any segment.
        let c = route(
            "public",
            json!({ "path": "/access/{what}", "methods": ["POST"] }),
        );
        errors_at(c, &at);

        // Prefixes and longer paths don't match.
        let c = with_routes(
            "public",
            json!([
                { "path": "/access", "methods": ["GET"] },
                { "path": "/access/routes/{id}", "methods": ["GET"] }
            ]),
        );
        run(c).unwrap();
    }

    /// A piece of identity's service file, as `plat5 routes generate` writes it.
    fn identity() -> Value {
        json!({
            "url": "http://identity:3000",
            "scopes": {
                "organization": {
                    "upstream_prefix": "/nodes/{organization.id}",
                    "routes": [
                        { "path": "/", "methods": {
                            "GET": { "required_permissions": ["organization:read"] },
                            "PATCH": { "required_permissions": ["organization:write"] }
                        } },
                        { "path": "/membership", "upstream": "/nodes/{organization.id}/memberships/{member.id}",
                          "methods": {
                            "GET": { "required_permissions": "any_member" },
                            "DELETE": { "required_permissions": "any_member" }
                        } }
                    ]
                },
                "user": { "routes": [
                    { "path": "/orgs", "upstream": "/roots/organization", "methods": ["POST"] },
                    { "path": "/user", "upstream": "/users/{user.id}", "methods": ["GET", "PATCH", "DELETE"] }
                ] }
            }
        })
    }

    // Identity is a service like any other: its file is what's served.
    #[test]
    fn identity_routes_are_served_as_written() {
        let mut c = base();
        c["services"] = json!({ "identity": identity() });
        run(c.clone()).unwrap();
        let config: Config = serde_json::from_value(c).unwrap();
        let routes = &crate::expand::expand(&config)["identity"].routes;
        let leave = routes
            .iter()
            .find(|r| r.method == "DELETE" && r.path == "/orgs/{organization}/membership")
            .unwrap();
        assert_eq!(
            leave.upstream,
            "/nodes/{organization.id}/memberships/{member.id}"
        );
        assert_eq!(leave.rate_limit, vec!["standard"]);
        assert!(routes
            .iter()
            .all(|r| !(r.method == "POST" && r.path.ends_with("/members"))));
        // The old resource overrides are unknown fields.
        let mut c = base();
        c["services"] = json!({ "identity": { "url": "http://identity:3000", "resources": { "invites": false } } });
        assert!(crate::schema::check_config(&c).is_err());
        assert!(serde_json::from_value::<Config>(c).is_err());
    }

    // A kind's list_permission is <kind name>:read when it names none.
    #[test]
    fn list_permission_defaults_to_the_kinds_read() {
        let mut c = base();
        c["kinds"]["project"]
            .as_object_mut()
            .unwrap()
            .remove("list_permission");
        c["kinds"]["environment"] = json!({
            "parent": "project", "path": "/envs/{environment}", "list_permission": "project:read"
        });
        c["roles"]["admin"]["bindable_at"] = json!(["organization", "project", "environment"]);
        run(c.clone()).unwrap();
        let config: Config = serde_json::from_value(c.clone()).unwrap();
        assert_eq!(config.kinds["project"].list_permission, "project:read");
        assert_eq!(config.kinds["environment"].list_permission, "project:read");
        // permission_name is gone: an unknown field.
        c["kinds"]["environment"]["permission_name"] = json!("project");
        assert!(crate::schema::check_config(&c).is_err());
        assert!(serde_json::from_value::<Config>(c).is_err());
    }

    // bind_creator is on unless a kind turns it off, and is sent only when off.
    #[test]
    fn bind_creator_defaults_on() {
        let mut c = base();
        c["kinds"]["project"]["bind_creator"] = json!(false);
        run(c.clone()).unwrap();
        let config: Config = serde_json::from_value(c).unwrap();
        assert!(config.kinds["organization"].bind_creator);
        assert!(!config.kinds["project"].bind_creator);
        let sent = serde_json::to_value(&config.kinds).unwrap();
        assert_eq!(sent["organization"].get("bind_creator"), None);
        assert_eq!(sent["project"]["bind_creator"], json!(false));
    }
}
