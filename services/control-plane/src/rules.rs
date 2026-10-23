//! What the schema can't say: the seven cross-document rules
//! (docs/concepts/config.md#validation-at-apply), the route shape rules that need
//! the joined path (docs/reference/config/routes.md#validation), shared rate
//! limits, and route ownership.

use std::collections::{BTreeMap, BTreeSet, HashMap};

use crate::config::{join_prefix, Config, Route, Service, PUBLIC_SCOPE};
use crate::schema::FieldError;

const MAX_PATH_LEN: usize = 1024;
const MAX_PATH_PARAMS: usize = 32;
const MAX_PARAM_NAME_LEN: usize = 64;

/// The config breaks a rule (**422**), or two services own one route (**409**).
#[derive(Debug, PartialEq)]
pub enum RuleError {
    Invalid(Vec<FieldError>),
    RouteConflict(Vec<String>),
}

/// Every rule, over a config that already passed the schema.
pub fn check(config: &Config) -> Result<(), RuleError> {
    let mut errs = Vec::new();
    check_kinds(config, &mut errs);
    // The rest reads the tree; with a broken tree its errors are noise.
    if errs.is_empty() {
        check_roles(config, &mut errs);
        for (name, service) in &config.services {
            check_service(config, name, service, &mut errs);
        }
        check_shared_policies(config, &mut errs);
    }
    if !errs.is_empty() {
        return Err(RuleError::Invalid(errs));
    }
    let conflicts = route_conflicts(config);
    if !conflicts.is_empty() {
        return Err(RuleError::RouteConflict(conflicts));
    }
    Ok(())
}

/// Rules 1 and 2: the tree. Also what `POST /catalog` checks.
pub fn check_kinds(config: &Config, errs: &mut Vec<FieldError>) {
    let sign_in: Vec<&String> = config
        .kinds
        .iter()
        .filter(|(_, k)| k.is_sign_in())
        .map(|(n, _)| n)
        .collect();
    match sign_in.as_slice() {
        [] => errs.push(FieldError::new(
            "kinds",
            "exactly one kind must have sign_in; none does",
        )),
        [one] => {
            if !config.kinds[*one].is_root() {
                errs.push(FieldError::new(
                    format!("kinds.{one}.parent"),
                    "the sign-in kind has no parent",
                ));
            }
        }
        many => errs.push(FieldError::new(
            "kinds",
            format!(
                "exactly one kind must have sign_in; {} do",
                many.iter()
                    .map(|s| s.as_str())
                    .collect::<Vec<_>>()
                    .join(", ")
            ),
        )),
    }
    for (name, kind) in &config.kinds {
        for parent in kind.parents() {
            let path = format!("kinds.{name}.parent");
            match config.kinds.get(&parent) {
                None => errs.push(FieldError::new(
                    path,
                    format!("parent {parent:?} is not a kind"),
                )),
                Some(p) if p.is_sign_in() => errs.push(FieldError::new(
                    path,
                    format!("{parent:?} is the sign-in kind; nothing sits under it"),
                )),
                Some(_) => {}
            }
        }
    }
    if let Some(cycle) = find_cycle(config) {
        errs.push(FieldError::new(
            format!("kinds.{}.parent", cycle[0]),
            format!("parents form a cycle: {}", cycle.join(" → ")),
        ));
    }
}

fn find_cycle(config: &Config) -> Option<Vec<String>> {
    fn visit(
        config: &Config,
        kind: &str,
        stack: &mut Vec<String>,
        done: &mut BTreeSet<String>,
    ) -> Option<Vec<String>> {
        if let Some(at) = stack.iter().position(|k| k == kind) {
            let mut cycle = stack[at..].to_vec();
            cycle.push(kind.to_string());
            return Some(cycle);
        }
        if done.contains(kind) {
            return None;
        }
        stack.push(kind.to_string());
        if let Some(k) = config.kinds.get(kind) {
            for parent in k.parents() {
                if let Some(c) = visit(config, &parent, stack, done) {
                    return Some(c);
                }
            }
        }
        stack.pop();
        done.insert(kind.to_string());
        None
    }
    let mut done = BTreeSet::new();
    for name in config.kinds.keys() {
        if let Some(c) = visit(config, name, &mut Vec::new(), &mut done) {
            return Some(c);
        }
    }
    None
}

/// Rules 3 and 4: where roles bind, and the two defaults.
fn check_roles(config: &Config, errs: &mut Vec<FieldError>) {
    let sign_in = config.sign_in_kind();
    for (slug, role) in &config.roles {
        for (i, kind) in role.bindable_at.iter().enumerate() {
            let path = format!("roles.{slug}.bindable_at[{i}]");
            if !config.kinds.contains_key(kind) {
                errs.push(FieldError::new(path, format!("{kind:?} is not a kind")));
            } else if Some(kind.as_str()) == sign_in {
                errs.push(FieldError::new(
                    path,
                    format!("{kind:?} is the sign-in kind; no role binds there"),
                ));
            }
        }
    }
    let mut default = |field: &str, slug: &str, kinds: Vec<&str>, what: &str| {
        let Some(role) = config.roles.get(slug) else {
            errs.push(FieldError::new(field, format!("{slug:?} is not a role")));
            return;
        };
        let missing: Vec<&str> = kinds
            .into_iter()
            .filter(|k| !role.bindable_at.iter().any(|b| b == k))
            .collect();
        if !missing.is_empty() {
            errs.push(FieldError::new(
                field,
                format!(
                    "{slug:?} must be bindable at every {what}; it isn't at {}",
                    missing.join(", ")
                ),
            ));
        }
        if role.max_duration.is_some() {
            errs.push(FieldError::new(
                field,
                format!("{slug:?} has a max_duration; {field} can't"),
            ));
        }
    };
    let roots: Vec<&str> = config
        .kinds
        .iter()
        .filter(|(_, k)| k.is_root() && !k.is_sign_in())
        .map(|(n, _)| n.as_str())
        .collect();
    default(
        "default_role",
        &config.default_role,
        roots,
        "root kind other than the sign-in kind",
    );
    default(
        "creator_role",
        &config.creator_role,
        config.creatable_kinds().collect(),
        "kind that can be created",
    );
}

/// What a route's scope gives it: the chain of kinds in its path, and the
/// subject fields its upstream may use.
struct Scope {
    chain: Vec<String>,
    sign_in: bool,
    public: bool,
}

fn check_service(config: &Config, name: &str, service: &Service, errs: &mut Vec<FieldError>) {
    for (policy, p) in &service.rate_limits {
        if let Some(kind) = &p.count_at {
            if !config.kinds.contains_key(kind) {
                errs.push(FieldError::new(
                    format!("services.{name}.rate_limits.{policy}.count_at"),
                    format!("{kind:?} is not a kind"),
                ));
            }
        }
    }
    if let Some(prefix) = &service.route_prefix {
        if prefix.contains(['{', '}']) {
            errs.push(FieldError::new(
                format!("services.{name}.route_prefix"),
                "route_prefix can't hold params",
            ));
        }
    }
    let mut seen: HashMap<(String, String), usize> = HashMap::new();
    for (i, route) in service.routes.iter().enumerate() {
        let at = format!("services.{name}.routes[{i}]");
        let path = join_prefix(service.route_prefix.as_deref(), &route.path);
        check_route(config, service, route, &path, &at, errs);
        for spec in route.method_specs() {
            let key = (path.clone(), spec.method.to_string());
            if let Some(first) = seen.insert(key, i) {
                errs.push(FieldError::new(
                    format!("{at}.methods"),
                    format!(
                        "{} {path} is already routes[{first}]; each path and method once",
                        spec.method
                    ),
                ));
            }
        }
    }
}

fn check_route(
    config: &Config,
    service: &Service,
    route: &Route,
    path: &str,
    at: &str,
    errs: &mut Vec<FieldError>,
) {
    let params = match check_path(path) {
        Ok(params) => params,
        Err(msg) => {
            errs.push(FieldError::new(format!("{at}.path"), msg));
            return;
        }
    };

    // Rule 5: scope, chain params, and the upstream's subject fields.
    let sign_in_kind = config.sign_in_kind().unwrap_or_default();
    let scope = if route.scope == PUBLIC_SCOPE {
        Scope {
            chain: Vec::new(),
            sign_in: false,
            public: true,
        }
    } else if route.scope == sign_in_kind {
        Scope {
            chain: Vec::new(),
            sign_in: true,
            public: false,
        }
    } else if config.kinds.contains_key(&route.scope) {
        let chain: Vec<String> = params
            .iter()
            .filter(|p| config.kinds.contains_key(*p))
            .cloned()
            .collect();
        if !config.chains(&route.scope).contains(&chain) {
            let want: Vec<String> = config
                .chains(&route.scope)
                .iter()
                .map(|c| {
                    c.iter()
                        .map(|k| format!("{{{k}}}"))
                        .collect::<Vec<_>>()
                        .join(" ")
                })
                .collect();
            errs.push(FieldError::new(
                format!("{at}.path"),
                format!(
                    "a {} route's path holds its chain's kind params, root first: {}",
                    route.scope,
                    want.join(", or ")
                ),
            ));
            return;
        }
        Scope {
            chain,
            sign_in: false,
            public: false,
        }
    } else {
        errs.push(FieldError::new(
            format!("{at}.scope"),
            format!("{:?} is not a kind or public", route.scope),
        ));
        return;
    };
    if scope.public || scope.sign_in {
        if let Some(kind) = params.iter().find(|p| config.kinds.contains_key(*p)) {
            errs.push(FieldError::new(
                format!("{at}.path"),
                format!(
                    "{{{kind}}} names a kind; a {} route has no chain",
                    route.scope
                ),
            ));
            return;
        }
    }
    check_upstream(config, route, &params, &scope, at, errs);

    // Rule 6: labels stated on kind routes, and only `[]` elsewhere.
    for spec in route.method_specs() {
        let field = match route.methods {
            crate::config::Methods::List(_) => format!("{at}.required_labels"),
            crate::config::Methods::Map(_) => {
                format!("{at}.methods.{}.required_labels", spec.method)
            }
        };
        match (scope.public || scope.sign_in, spec.required_labels) {
            (false, None) => errs.push(FieldError::new(
                field,
                format!(
                    "required for {} on a {} route; [] means any member of the tree",
                    spec.method, route.scope
                ),
            )),
            (true, Some(labels)) if !labels.is_empty() => errs.push(FieldError::new(
                field,
                format!(
                    "a {} route takes no labels; omit them or use []",
                    route.scope
                ),
            )),
            _ => {}
        }
        if scope.public && spec.audit.is_some() {
            let field = match route.methods {
                crate::config::Methods::List(_) => format!("{at}.audit"),
                crate::config::Methods::Map(_) => format!("{at}.methods.{}.audit", spec.method),
            };
            errs.push(FieldError::new(field, "a public route has no audit log"));
        }
        // Rule 7: named policies exist, and count at a node the route has.
        if let Some(rl) = spec.rate_limit {
            let field = match route.methods {
                crate::config::Methods::List(_) => format!("{at}.rate_limit"),
                crate::config::Methods::Map(_) => {
                    format!("{at}.methods.{}.rate_limit", spec.method)
                }
            };
            for name in rl.names() {
                let Some(policy) = service.rate_limits.get(name) else {
                    errs.push(FieldError::new(
                        field.clone(),
                        format!("{name:?} is not in this service's rate_limits"),
                    ));
                    continue;
                };
                let Some(count_at) = &policy.count_at else {
                    continue;
                };
                let ok = if scope.public {
                    false
                } else if scope.sign_in {
                    count_at == sign_in_kind
                } else {
                    scope.chain.contains(count_at)
                };
                if !ok {
                    errs.push(FieldError::new(
                        field.clone(),
                        format!(
                            "{name:?} counts at {count_at}, which isn't this {} route's kind or an ancestor",
                            route.scope
                        ),
                    ));
                }
            }
        }
    }

    if route.resolve_deleted.is_some() && (scope.public || scope.sign_in) {
        errs.push(FieldError::new(
            format!("{at}.resolve_deleted"),
            format!(
                "only on routes scoped to a kind; this one is {}",
                route.scope
            ),
        ));
    }
}

fn check_upstream(
    config: &Config,
    route: &Route,
    params: &[String],
    scope: &Scope,
    at: &str,
    errs: &mut Vec<FieldError>,
) {
    let field = format!("{at}.upstream");
    let Some(upstream) = &route.upstream else {
        if !scope.chain.is_empty() {
            errs.push(FieldError::new(
                field,
                "required on a path with chain params: slugs never reach a service",
            ));
        }
        return;
    };
    let placeholders = match placeholders(upstream) {
        Ok(p) => p,
        Err(msg) => {
            errs.push(FieldError::new(field, msg));
            return;
        }
    };
    for (namespace, name) in placeholders {
        match namespace.as_str() {
            "subject" => {
                let ok = if name == "member_id" {
                    !scope.public
                } else {
                    name.strip_suffix("_id")
                        .is_some_and(|kind| scope.chain.iter().any(|k| k == kind))
                };
                if !ok {
                    errs.push(FieldError::new(
                        field.clone(),
                        format!("a {} route has no subject.{name}", route.scope),
                    ));
                }
            }
            "path" => {
                if config.kinds.contains_key(&name) && params.contains(&name) {
                    errs.push(FieldError::new(
                        field.clone(),
                        format!("path.{name} is a chain param; use subject.{name}_id (slugs never reach a service)"),
                    ));
                } else if !params.contains(&name) {
                    errs.push(FieldError::new(
                        field.clone(),
                        format!("path.{name} is not a param of the path"),
                    ));
                }
            }
            other => errs.push(FieldError::new(
                field.clone(),
                format!("unknown namespace {other:?}; use subject.* or path.*"),
            )),
        }
    }
}

/// A path's params, once its canonical form is checked: no trailing `/` (but
/// `/`), lowercase outside params, visible ASCII, at most 1024 bytes and 32
/// params, each named `[A-Za-z_][A-Za-z0-9_]*`, at most 64.
pub fn check_path(path: &str) -> Result<Vec<String>, String> {
    if !path.starts_with('/') {
        return Err("must start with /".into());
    }
    if path.len() > MAX_PATH_LEN {
        return Err(format!("longer than {MAX_PATH_LEN} bytes"));
    }
    if !path.bytes().all(|b| b.is_ascii_graphic()) {
        return Err("must be visible ASCII: no spaces; percent-encode anything else".into());
    }
    if path.len() > 1 && path.ends_with('/') {
        return Err(format!(
            "must not end with / (use {})",
            path.trim_end_matches('/')
        ));
    }
    let mut params = Vec::new();
    let mut rest = path;
    let mut literal = String::new();
    while let Some(start) = rest.find('{') {
        literal.push_str(&rest[..start]);
        let after = &rest[start + 1..];
        let Some(end) = after.find('}') else {
            return Err("unclosed {".into());
        };
        let name = &after[..end];
        if !valid_param_name(name) {
            return Err(format!(
                "param {{{name}}} must be 1-{MAX_PARAM_NAME_LEN} of letters, digits or _, not starting with a digit"
            ));
        }
        if params.iter().any(|p| p == name) {
            return Err(format!("param {{{name}}} appears twice"));
        }
        params.push(name.to_string());
        rest = &after[end + 1..];
    }
    literal.push_str(rest);
    if literal.contains('}') {
        return Err("unopened }".into());
    }
    if literal.chars().any(|c| c.is_ascii_uppercase()) {
        return Err("must be lowercase outside {params}".into());
    }
    if params.len() > MAX_PATH_PARAMS {
        return Err(format!("more than {MAX_PATH_PARAMS} params"));
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

/// `{namespace.name}` placeholders in an upstream template. A bare `{name}` is
/// refused: the namespace is the point.
fn placeholders(template: &str) -> Result<Vec<(String, String)>, String> {
    let mut out = Vec::new();
    let mut rest = template;
    while let Some(start) = rest.find('{') {
        let after = &rest[start + 1..];
        let Some(end) = after.find('}') else {
            return Err("unclosed {".into());
        };
        let body = &after[..end];
        let Some((namespace, name)) = body.split_once('.') else {
            return Err(format!("bare {{{body}}}; use subject.* or path.*"));
        };
        if namespace.is_empty() || name.is_empty() || name.contains(['.', '{']) {
            return Err(format!("invalid placeholder {{{body}}}"));
        }
        out.push((namespace.to_string(), name.to_string()));
        rest = &after[end + 1..];
    }
    if rest.contains('}') {
        return Err("unopened }".into());
    }
    Ok(out)
}

/// A shared policy name means the same bucket everywhere, so every service that
/// declares it must agree; one shared and one local is refused.
fn check_shared_policies(config: &Config, errs: &mut Vec<FieldError>) {
    let mut by_name: BTreeMap<&str, Vec<(&str, &crate::config::Policy)>> = BTreeMap::new();
    for (service, s) in &config.services {
        for (name, policy) in &s.rate_limits {
            by_name.entry(name).or_default().push((service, policy));
        }
    }
    for (name, entries) in by_name {
        if !entries.iter().any(|(_, p)| p.shared) {
            continue;
        }
        let (first_service, first) = entries[0];
        for (service, policy) in &entries[1..] {
            if *policy != first {
                errs.push(FieldError::new(
                    format!("services.{service}.rate_limits.{name}"),
                    format!(
                        "shared policy {name:?} differs from services.{first_service}: every service that uses it declares the same entry, with shared: true"
                    ),
                ));
            }
        }
    }
}

/// A path's matching shape: each `{param}` is `{}`; literals stay.
fn path_shape(path: &str) -> String {
    let mut out = String::with_capacity(path.len());
    let mut rest = path;
    while let Some(start) = rest.find('{') {
        out.push_str(&rest[..start]);
        let after = &rest[start + 1..];
        let Some(end) = after.find('}') else {
            out.push_str(&rest[start..]);
            return out;
        };
        out.push_str("{}");
        rest = &after[end + 1..];
    }
    out.push_str(rest);
    out
}

/// Each method and path shape owned by two services, as
/// `"GET /a/{x} (services a and b)"`, sorted. Shapes within one service never
/// conflict (their duplicates are a rule error instead).
pub fn route_conflicts(config: &Config) -> Vec<String> {
    let mut owners: BTreeMap<(String, String), BTreeSet<&str>> = BTreeMap::new();
    let mut seen_path: BTreeMap<(String, String), BTreeSet<String>> = BTreeMap::new();
    for (name, service) in &config.services {
        for route in &service.routes {
            let path = join_prefix(service.route_prefix.as_deref(), &route.path);
            for spec in route.method_specs() {
                let key = (spec.method.to_string(), path_shape(&path));
                owners.entry(key.clone()).or_default().insert(name);
                seen_path.entry(key).or_default().insert(path.clone());
            }
        }
    }
    owners
        .into_iter()
        .filter(|(_, services)| services.len() > 1)
        .map(|(key, services)| {
            let paths: Vec<String> = seen_path[&key].iter().cloned().collect();
            format!(
                "{} {} (services {})",
                key.0,
                paths.join(" / "),
                services.into_iter().collect::<Vec<_>>().join(" and ")
            )
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::{json, Value};

    pub(crate) fn base() -> Value {
        json!({
            "kinds": {
                "directory": { "sign_in": "idp" },
                "organization": { "label": "Organization", "segment": "orgs" },
                "folder": { "parent": "organization" },
                "project": { "parent": ["organization", "folder"] }
            },
            "roles": {
                "admin": { "labels": ["*"], "bindable_at": ["organization", "folder", "project"] },
                "member": { "labels": ["organization:read"], "bindable_at": ["organization"] },
                "support": { "labels": ["project:read"], "bindable_at": ["organization"], "max_duration": "7d" }
            },
            "default_role": "member",
            "creator_role": "admin"
        })
    }

    fn run(v: Value) -> Result<(), RuleError> {
        crate::schema::check_config(&v).unwrap_or_else(|e| panic!("schema: {e:?}"));
        let config: Config = serde_json::from_value(v).unwrap();
        check(&config)
    }

    fn invalid_at(v: Value, path: &str) {
        match run(v) {
            Err(RuleError::Invalid(errs)) => {
                assert!(errs.iter().any(|e| e.path == path), "want {path}: {errs:?}")
            }
            other => panic!("want invalid at {path}, got {other:?}"),
        }
    }

    fn with_routes(routes: Value) -> Value {
        let mut c = base();
        c["services"] = json!({ "e10s": {
            "url": "http://e10s:3000",
            "rate_limits": {
                "per-project": { "requests": 100, "window_seconds": 1, "count_at": "project" },
                "per-org": { "requests": 300, "window_seconds": 1, "count_at": "organization" },
                "per-dir": { "requests": 10, "window_seconds": 1, "count_at": "directory" }
            },
            "routes": routes
        } });
        c
    }

    #[test]
    fn the_base_config_holds() {
        run(base()).unwrap();
    }

    #[test]
    fn rule_1_one_sign_in_kind_with_no_parent_and_no_children() {
        let mut c = base();
        c["kinds"]["directory"] = json!({});
        invalid_at(c, "kinds");
        let mut c = base();
        c["kinds"]["people"] = json!({ "sign_in": "idp" });
        invalid_at(c, "kinds");
        let mut c = base();
        c["kinds"]["directory"]["parent"] = json!("organization");
        invalid_at(c, "kinds.directory.parent");
        let mut c = base();
        c["kinds"]["team"] = json!({ "parent": "directory" });
        invalid_at(c, "kinds.team.parent");
    }

    #[test]
    fn rule_2_parents_exist_without_cycles() {
        let mut c = base();
        c["kinds"]["project"]["parent"] = json!("workspace");
        invalid_at(c, "kinds.project.parent");
        let mut c = base();
        c["kinds"]["organization"]["parent"] = json!("project");
        match run(c) {
            Err(RuleError::Invalid(errs)) => {
                assert!(errs.iter().any(|e| e.message.contains("cycle")), "{errs:?}")
            }
            other => panic!("{other:?}"),
        }
    }

    #[test]
    fn rule_3_bindable_at_real_kinds_not_sign_in() {
        let mut c = base();
        c["roles"]["admin"]["bindable_at"] = json!(["organization", "folder", "project", "team"]);
        invalid_at(c, "roles.admin.bindable_at[3]");
        let mut c = base();
        c["roles"]["member"]["bindable_at"] = json!(["organization", "directory"]);
        invalid_at(c, "roles.member.bindable_at[1]");
    }

    #[test]
    fn rule_4_defaults() {
        let mut c = base();
        c["default_role"] = json!("nobody");
        invalid_at(c, "default_role");
        let mut c = base();
        c["roles"]["member"]["bindable_at"] = json!(["project"]);
        invalid_at(c, "default_role");
        let mut c = base();
        c["creator_role"] = json!("member");
        invalid_at(c, "creator_role");
        let mut c = base();
        c["default_role"] = json!("support");
        invalid_at(c, "default_role");
    }

    #[test]
    fn rule_5_chain_params_and_subject_fields() {
        let ok = json!([
            { "path": "/orgs/{organization}/projects/{project}/x/{id}", "scope": "project",
              "upstream": "/projects/{subject.project_id}/orgs/{subject.organization_id}/x/{path.id}/{subject.member_id}",
              "methods": ["GET"], "required_labels": [] },
            { "path": "/orgs/{organization}/folders/{folder}/projects/{project}/x", "scope": "project",
              "upstream": "/projects/{subject.project_id}/{subject.folder_id}",
              "methods": ["GET"], "required_labels": [] },
            { "path": "/me/x", "scope": "directory", "upstream": "/people/{subject.member_id}", "methods": ["GET"] },
            { "path": "/health", "scope": "public", "methods": ["GET"] }
        ]);
        run(with_routes(ok)).unwrap();

        let bad = |route: Value, path: &str| invalid_at(with_routes(json!([route])), path);
        bad(
            json!({ "path": "/x", "scope": "team", "methods": ["GET"] }),
            "services.e10s.routes[0].scope",
        );
        // Missing the root.
        bad(
            json!({ "path": "/projects/{project}", "scope": "project", "upstream": "/p/{subject.project_id}",
                    "methods": ["GET"], "required_labels": [] }),
            "services.e10s.routes[0].path",
        );
        // Out of order.
        bad(
            json!({ "path": "/p/{project}/o/{organization}", "scope": "project", "upstream": "/p",
                    "methods": ["GET"], "required_labels": [] }),
            "services.e10s.routes[0].path",
        );
        // A kind outside the chain.
        bad(
            json!({ "path": "/o/{organization}/p/{project}/f/{folder}", "scope": "project", "upstream": "/p",
                    "methods": ["GET"], "required_labels": [] }),
            "services.e10s.routes[0].path",
        );
        // A kind param on a public route.
        bad(
            json!({ "path": "/o/{organization}", "scope": "public", "upstream": "/o", "methods": ["GET"] }),
            "services.e10s.routes[0].path",
        );
        // Subject fields the scope doesn't give.
        bad(
            json!({ "path": "/o/{organization}", "scope": "organization", "upstream": "/p/{subject.project_id}",
                    "methods": ["GET"], "required_labels": [] }),
            "services.e10s.routes[0].upstream",
        );
        bad(
            json!({ "path": "/me", "scope": "directory", "upstream": "/o/{subject.organization_id}", "methods": ["GET"] }),
            "services.e10s.routes[0].upstream",
        );
        bad(
            json!({ "path": "/h", "scope": "public", "upstream": "/h/{subject.member_id}", "methods": ["GET"] }),
            "services.e10s.routes[0].upstream",
        );
        // Slugs never reach a service.
        bad(
            json!({ "path": "/o/{organization}", "scope": "organization", "upstream": "/o/{path.organization}",
                    "methods": ["GET"], "required_labels": [] }),
            "services.e10s.routes[0].upstream",
        );
        bad(
            json!({ "path": "/o/{organization}", "scope": "organization", "methods": ["GET"], "required_labels": [] }),
            "services.e10s.routes[0].upstream",
        );
        bad(
            json!({ "path": "/h/{id}", "scope": "public", "upstream": "/h/{id}", "methods": ["GET"] }),
            "services.e10s.routes[0].upstream",
        );
        bad(
            json!({ "path": "/h/{id}", "scope": "public", "upstream": "/h/{path.other}", "methods": ["GET"] }),
            "services.e10s.routes[0].upstream",
        );
    }

    #[test]
    fn rule_6_labels_stated() {
        let bad = |route: Value, path: &str| invalid_at(with_routes(json!([route])), path);
        bad(
            json!({ "path": "/o/{organization}", "scope": "organization", "upstream": "/o/{subject.organization_id}",
                    "methods": ["GET"] }),
            "services.e10s.routes[0].required_labels",
        );
        bad(
            json!({ "path": "/o/{organization}", "scope": "organization", "upstream": "/o/{subject.organization_id}",
                    "methods": { "GET": { "required_labels": [] }, "POST": null } }),
            "services.e10s.routes[0].methods.POST.required_labels",
        );
        bad(
            json!({ "path": "/h", "scope": "public", "methods": ["GET"], "required_labels": ["x"] }),
            "services.e10s.routes[0].required_labels",
        );
        bad(
            json!({ "path": "/me", "scope": "directory", "methods": { "GET": { "required_labels": ["x"] } } }),
            "services.e10s.routes[0].methods.GET.required_labels",
        );
        run(with_routes(json!([
            { "path": "/h", "scope": "public", "methods": ["GET"], "required_labels": [] }
        ])))
        .unwrap();
    }

    #[test]
    fn rule_7_rate_limits_count_on_the_chain() {
        let project = |rl: Value| {
            json!({ "path": "/o/{organization}/p/{project}", "scope": "project", "upstream": "/p/{subject.project_id}",
                    "methods": { "POST": { "required_labels": ["check"], "rate_limit": rl } } })
        };
        run(with_routes(json!([project(json!([
            "per-project",
            "per-org"
        ]))])))
        .unwrap();
        invalid_at(
            with_routes(json!([project(json!("nope"))])),
            "services.e10s.routes[0].methods.POST.rate_limit",
        );
        let org = json!({ "path": "/o/{organization}", "scope": "organization", "upstream": "/o/{subject.organization_id}",
                          "methods": ["GET"], "required_labels": [], "rate_limit": "per-project" });
        invalid_at(
            with_routes(json!([org])),
            "services.e10s.routes[0].rate_limit",
        );
        let public =
            json!({ "path": "/h", "scope": "public", "methods": ["GET"], "rate_limit": "per-org" });
        invalid_at(
            with_routes(json!([public])),
            "services.e10s.routes[0].rate_limit",
        );
        run(with_routes(json!([
            { "path": "/me", "scope": "directory", "methods": ["GET"], "rate_limit": "per-dir" }
        ])))
        .unwrap();
        let mut c = with_routes(json!([{ "path": "/h", "scope": "public", "methods": ["GET"] }]));
        c["services"]["e10s"]["rate_limits"]["per-team"] =
            json!({ "requests": 1, "window_seconds": 1, "count_at": "team" });
        invalid_at(c, "services.e10s.rate_limits.per-team.count_at");
    }

    #[test]
    fn route_shape_rules() {
        let bad = |route: Value, path: &str| invalid_at(with_routes(json!([route])), path);
        for p in ["/H", "/h/", "/h/{x", "/h/{1x}", "/h/{x}/{x}"] {
            bad(
                json!({ "path": p, "scope": "public", "methods": ["GET"] }),
                "services.e10s.routes[0].path",
            );
        }
        bad(
            json!({ "path": "/h", "scope": "public", "methods": ["GET"], "audit": true }),
            "services.e10s.routes[0].audit",
        );
        bad(
            json!({ "path": "/me", "scope": "directory", "methods": ["POST"], "resolve_deleted": true }),
            "services.e10s.routes[0].resolve_deleted",
        );
        invalid_at(
            with_routes(json!([
                { "path": "/h", "scope": "public", "methods": ["GET", "POST"] },
                { "path": "/h", "scope": "public", "methods": { "GET": null } }
            ])),
            "services.e10s.routes[1].methods",
        );
        // route_prefix joins before the checks.
        let mut c = with_routes(
            json!([{ "path": "/", "scope": "public", "methods": ["GET"] },
                                         { "path": "/a", "scope": "public", "methods": ["GET"] }]),
        );
        c["services"]["e10s"]["route_prefix"] = json!("/e10s/");
        let config: Config = serde_json::from_value(c.clone()).unwrap();
        let paths: Vec<String> = config.services["e10s"]
            .stored()
            .routes
            .into_iter()
            .map(|r| r.path)
            .collect();
        assert_eq!(paths, ["/e10s", "/e10s/a"]);
        run(c).unwrap();
    }

    #[test]
    fn shared_policies_agree() {
        let svc = |shared: Value| {
            json!({ "url": "http://s:1", "rate_limits": { "global": shared },
                    "routes": [{ "path": format!("/{}", rand_path()), "scope": "public", "methods": ["GET"] }] })
        };
        let mut c = base();
        c["services"] = json!({
            "a": svc(json!({ "requests": 1, "window_seconds": 1, "shared": true })),
            "b": svc(json!({ "requests": 2, "window_seconds": 1, "shared": true }))
        });
        invalid_at(c, "services.b.rate_limits.global");
        let mut c = base();
        c["services"] = json!({
            "a": svc(json!({ "requests": 1, "window_seconds": 1, "shared": true })),
            "b": svc(json!({ "requests": 1, "window_seconds": 1 }))
        });
        invalid_at(c, "services.b.rate_limits.global");
    }

    fn rand_path() -> String {
        use std::sync::atomic::{AtomicUsize, Ordering};
        static N: AtomicUsize = AtomicUsize::new(0);
        format!("p{}", N.fetch_add(1, Ordering::Relaxed))
    }

    #[test]
    fn two_services_on_one_shape_conflict() {
        let mut c = base();
        c["services"] = json!({
            "a": { "url": "http://a:1", "routes": [{ "path": "/x/{id}", "scope": "public", "methods": ["GET", "POST"] }] },
            "b": { "url": "http://b:1", "routes": [
                { "path": "/x/{other}", "scope": "public", "methods": ["GET"] },
                { "path": "/x/literal", "scope": "public", "methods": ["POST"] }
            ] }
        });
        assert_eq!(
            run(c),
            Err(RuleError::RouteConflict(vec![
                "GET /x/{id} / /x/{other} (services a and b)".into()
            ]))
        );
    }
}
