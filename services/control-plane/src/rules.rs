//! What the schema can't say: the rules that tie the documents together
//! (docs/concepts/config.md#validation-at-apply) and the route rules
//! (docs/reference/config/services.md#validation), over every expanded route,
//! identity's and audit's included. The control plane is the one validator: the
//! gateway and identity trust what passes.

use std::collections::{BTreeMap, BTreeSet};

use crate::config::{Config, RateLimit, DEFAULT_POLICY, PUBLIC_SCOPE, USER_SCOPE};
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
        let specs = expand::specs(config, &mut errs);
        let mut all = Vec::new();
        let mut valid = Vec::new();
        for spec in &specs {
            let routes = expand::expand_spec(config, spec);
            if check_spec(config, spec, &mut errs) {
                valid.extend(routes.iter().cloned());
            }
            all.extend(routes);
        }
        check_shapes(&valid, &mut errs);
        check_roles(config, &all, &mut errs);
    }
    if errs.is_empty() {
        return Ok(());
    }
    let mut seen = BTreeSet::new();
    errs.retain(|e| seen.insert((e.path.clone(), e.message.clone())));
    Err(errs)
}

/// Rule 1: every parent is a kind, no cycles, and each kind's path param is
/// its own name.
fn check_kinds(config: &Config, errs: &mut Vec<FieldError>) {
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

/// A policy's `count_at` is a kind, and `default` has none.
fn check_policies(config: &Config, errs: &mut Vec<FieldError>) {
    for (name, policy) in &config.rate_limits {
        if let Some(kind) = &policy.count_at {
            if name == DEFAULT_POLICY {
                errs.push(FieldError::new(
                    format!("rate_limits.{name}"),
                    "the default policy applies to every route, so it can't have count_at",
                ));
            } else if !config.kinds.contains_key(kind) {
                errs.push(FieldError::new(
                    format!("rate_limits.{name}.count_at"),
                    format!("{kind:?} is not a kind"),
                ));
            }
        }
    }
}

/// Rule 2 and 3: roles bind at kinds and include roles, `creator_role` binds
/// everywhere, and every permission a role grants does something.
fn check_roles(config: &Config, routes: &[ExpandedRoute], errs: &mut Vec<FieldError>) {
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

    let mut required: BTreeSet<&str> = routes
        .iter()
        .filter_map(|r| r.required_permissions.as_ref())
        .flat_map(|r| r.permissions().iter().map(String::as_str))
        .collect();
    required.extend(config.kinds.values().map(|k| k.list_permission.as_str()));
    for (slug, role) in &config.roles {
        for (i, p) in role.permissions.iter().enumerate() {
            if p != "*" && !required.contains(p.as_str()) {
                errs.push(FieldError::new(
                    format!("roles.{slug}.permissions[{i}]"),
                    format!("no route requires {p:?}, and no kind lists by it: a typo, or a route that was removed"),
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
    if let Some(keeps) = &spec.keeps {
        let want: BTreeSet<&String> = keeps.iter().collect();
        let got: BTreeSet<&String> = params.iter().collect();
        if want != got {
            let want: Vec<String> = keeps.iter().map(|p| format!("{{{p}}}")).collect();
            err(
                errs,
                &spec.path_at,
                format!(
                    "an overridden path keeps the resource's params: {}",
                    if want.is_empty() {
                        "none".to_string()
                    } else {
                        want.join(", ")
                    }
                ),
            );
            ok = false;
        }
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
        let (names, names_at): (Vec<String>, String) = match &v.rate_limit {
            Some(r) => match &r.value {
                RateLimit::Policies(p) => (p.clone(), r.at.clone()),
                RateLimit::Off(_) => (Vec::new(), r.at.clone()),
            },
            // Omitted is `default`, which has no count_at to check.
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
            "rate_limits": {
                "default": { "requests": 60, "window_seconds": 60 },
                "per-project": { "requests": 100, "window_seconds": 1, "count_at": "project" },
                "per-org": { "requests": 300, "window_seconds": 1, "count_at": "organization" }
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

    #[test]
    fn a_permission_no_route_requires() {
        let mut c = base();
        c["roles"]["member"]["permissions"] = json!(["organization:read", "typo:read"]);
        errors_at(c, "roles.member.permissions[1]");
        // A kind's list_permission counts; so do identity's defaults.
        let mut c = base();
        c["roles"]["member"]["permissions"] = json!(["project:read"]);
        run(c).unwrap();
        let mut c = base();
        c["roles"]["member"]["permissions"] = json!(["project:bindings:write"]);
        errors_at(c.clone(), "roles.member.permissions[0]");
        c["services"] = json!({ "identity": { "url": "http://identity:3000" } });
        run(c).unwrap();
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
        c["rate_limits"]["per-team"] =
            json!({ "requests": 1, "window_seconds": 1, "count_at": "team" });
        errors_at(c, "rate_limits.per-team.count_at");
        let mut c = base();
        c["rate_limits"]["default"]["count_at"] = json!("organization");
        let errs = errors_at(c, "rate_limits.default");
        assert_eq!(
            errs[0].message,
            "the default policy applies to every route, so it can't have count_at"
        );
        // Omitted is `default`, in every scope; `false` is none.
        let c = with_routes(
            "public",
            json!([
                { "path": "/a", "methods": ["GET"] },
                { "path": "/b", "methods": ["GET"], "rate_limit": false }
            ]),
        );
        run(c.clone()).unwrap();
        let config: Config = serde_json::from_value(c).unwrap();
        let routes = &expand::expand(&config)["e10s"].routes;
        assert_eq!(routes[0].rate_limit, vec![DEFAULT_POLICY]);
        assert!(routes[1].rate_limit.is_empty());
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
        // Across services, and against identity's resources.
        let mut c = route(
            "organization",
            json!({ "path": "/", "methods": ["GET"], "required_permissions": ["organization:read"] }),
        );
        c["services"]["identity"] = json!({ "url": "http://identity:3000" });
        let errs = errors_at(
            c,
            "services.identity.kinds.organization.resources.node.path",
        );
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

    /// Identity's expanded public paths for one override set, method and path.
    fn identity_paths(identity: Value) -> Vec<String> {
        let mut s = json!({ "url": "http://identity:3000" });
        s.as_object_mut()
            .unwrap()
            .extend(identity.as_object().unwrap().clone());
        let c = with_identity(s);
        crate::schema::check_config(&c).unwrap_or_else(|e| panic!("schema: {e:?}"));
        let config: Config = serde_json::from_value(c).unwrap();
        check(&config).unwrap();
        crate::expand::expand(&config)["identity"]
            .routes
            .iter()
            .map(|r| format!("{} {}", r.method, r.path))
            .collect()
    }

    // `false` on a collection removes the routes below it; turning it back on
    // for one kind brings them back; an item's own entry at a layer wins.
    #[test]
    fn collection_false_removes_its_items() {
        let has = |paths: &[String], p: &str| paths.iter().any(|x| x == p);
        let org_sa = "/orgs/{organization}/service-accounts";
        let proj_sa = "/orgs/{organization}/projects/{project}/service-accounts";

        let paths = identity_paths(json!({
            "kinds": { "project": { "resources": { "service_accounts": false, "invites": false } } }
        }));
        for p in [
            format!("GET {proj_sa}"),
            format!("DELETE {proj_sa}/{{service_account_id}}"),
            format!("POST {proj_sa}/{{service_account_id}}/keys"),
            format!("DELETE {proj_sa}/{{service_account_id}}/keys/{{key_id}}"),
            "DELETE /orgs/{organization}/projects/{project}/invites/{invite_id}".to_string(),
        ] {
            assert!(!has(&paths, &p), "{p} kept: {paths:?}");
        }
        assert!(has(
            &paths,
            &format!("DELETE {org_sa}/{{service_account_id}}/keys/{{key_id}}")
        ));
        assert!(has(
            &paths,
            "DELETE /orgs/{organization}/invites/{invite_id}"
        ));

        // Off for every kind, back on for the org: the org's items come back.
        let paths = identity_paths(json!({
            "resources": { "service_accounts": false },
            "kinds": { "organization": { "resources": { "service_accounts": {} } } }
        }));
        assert!(has(
            &paths,
            &format!("POST {org_sa}/{{service_account_id}}/keys")
        ));
        assert!(!has(
            &paths,
            &format!("POST {proj_sa}/{{service_account_id}}/keys")
        ));

        // keys off removes key too; the service accounts stay.
        let paths = identity_paths(json!({ "resources": { "keys": false } }));
        assert!(has(&paths, &format!("GET {org_sa}/{{service_account_id}}")));
        assert!(!has(
            &paths,
            &format!("DELETE {org_sa}/{{service_account_id}}/keys/{{key_id}}")
        ));

        // The user scope: my_keys off removes my_key.
        let paths = identity_paths(json!({ "resources": { "my_keys": false } }));
        assert!(!has(&paths, "DELETE /user/keys/{key_id}"), "{paths:?}");
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

    // A kind's permission_name is what {kind} reads as in identity's
    // resources, and starts its default list_permission.
    #[test]
    fn permission_name_shared_with_the_parent() {
        let mut c = with_identity(json!({ "url": "http://identity:3000" }));
        c["kinds"]["environment"] = json!({
            "parent": "project", "path": "/envs/{environment}", "permission_name": "project"
        });
        c["kinds"]["project"]
            .as_object_mut()
            .unwrap()
            .remove("list_permission");
        c["roles"]["admin"]["bindable_at"] = json!(["organization", "project", "environment"]);
        c["roles"]["developer"] = json!({ "permissions": ["project:read", "project:write"], "bindable_at": ["environment"] });
        run(c.clone()).unwrap();
        let config: Config = serde_json::from_value(c.clone()).unwrap();
        assert_eq!(config.kinds["project"].list_permission, "project:read");
        assert_eq!(config.kinds["environment"].list_permission, "project:read");
        assert_eq!(
            config.kinds["organization"].list_permission,
            "organization:read"
        );

        let env = "/orgs/{organization}/projects/{project}/envs/{environment}";
        let required = |method: &str, path: &str| {
            let r = crate::expand::expand(&config)["identity"]
                .routes
                .iter()
                .find(|r| r.method == method && r.path == path)
                .unwrap_or_else(|| panic!("no {method} {path}"))
                .clone();
            r.required_permissions.unwrap().permissions().to_vec()
        };
        assert_eq!(required("PATCH", env), ["project:write"]);
        assert_eq!(
            required("POST", "/orgs/{organization}/projects/{project}/envs"),
            ["project:create"]
        );
        assert_eq!(
            required("GET", &format!("{env}/bindings")),
            ["project:bindings:read"]
        );
        assert_eq!(
            required("PATCH", "/orgs/{organization}/projects/{project}"),
            ["project:write"]
        );

        // Nothing requires the kind's own name any more.
        c["roles"]["developer"]["permissions"] = json!(["environment:read"]);
        errors_at(c, "roles.developer.permissions[0]");
    }

    fn with_identity(identity: Value) -> Value {
        let mut c = base();
        c["roles"]["member"]["permissions"] = json!(["organization:read"]);
        c["services"] = json!({ "identity": identity });
        c
    }

    #[test]
    fn identity_overrides() {
        let id = |extra: Value| {
            let mut s = json!({ "url": "http://identity:3000" });
            s.as_object_mut()
                .unwrap()
                .extend(extra.as_object().unwrap().clone());
            with_identity(s)
        };
        run(id(json!({
            "resources": { "binding": { "path": "/grants/{binding_id}" }, "invites": false },
            "kinds": { "project": { "resources": { "children": { "path": "/apps" } } } }
        })))
        .unwrap();
        let base_at = "services.identity";
        errors_at(
            id(json!({ "resources": { "binding": { "path": "/grants/{id}" } } })),
            &format!("{base_at}.resources.binding.path"),
        );
        errors_at(
            id(json!({ "kinds": { "team": { "resources": { "node": false } } } })),
            &format!("{base_at}.kinds.team"),
        );
        errors_at(
            id(json!({ "kinds": { "organization": { "resources": { "children": false } } } })),
            &format!("{base_at}.kinds.organization.resources.children"),
        );
        errors_at(
            id(json!({ "kinds": { "project": { "resources": { "members": false } } } })),
            &format!("{base_at}.kinds.project.resources.members"),
        );
        errors_at(
            id(json!({ "resources": { "node": { "methods": { "POST": null } } } })),
            &format!("{base_at}.resources.node.methods.POST"),
        );
        errors_at(
            id(
                json!({ "kinds": { "project": { "resources": { "membership": { "methods": { "DELETE": false } } } } } }),
            ),
            &format!("{base_at}.kinds.project.resources.membership.methods.DELETE"),
        );
        // membership's DELETE is on roots; a shared override of it is fine.
        run(id(
            json!({ "resources": { "membership": { "methods": { "DELETE": false } } } }),
        ))
        .unwrap();
        errors_at(
            id(json!({ "resources": { "me": { "required_permissions": ["organization:read"] } } })),
            &format!("{base_at}.resources.me.required_permissions"),
        );
        errors_at(
            id(json!({ "resources": { "node": { "rate_limit": ["ghost"] } } })),
            &format!("{base_at}.resources.node.rate_limit"),
        );
    }
}
