//! Path and template validation helpers.

use crate::config::Config;
use crate::schema::FieldError;

pub(super) const MAX_PATH_LEN: usize = 1024;
pub(super) const MAX_PATH_PARAMS: usize = 32;
const MAX_PARAM_NAME_LEN: usize = 64;

/// What a route's scope gives it.
pub(super) enum Scope {
    /// A kind, with its chain of kinds, root first.
    Kind(Vec<String>),
    User,
    Public,
}

/// The placeholders of an upstream or authorizer resource template: `{<kind>.id}`
/// for a kind in the chain, `{member.id}` in a non-public kind scope, `{user.id}`
/// in `user`, and in a route's `upstream`/`resource.id` (`own` is its params) a
/// bare `{param}` of the route's.
#[allow(clippy::too_many_arguments)]
pub(super) fn check_template(
    config: &Config,
    scope_name: &str,
    scope: &Scope,
    template: &str,
    own: Option<&[String]>,
    allow_public: bool,
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
                Scope::Kind(_) if allow_public => Some(
                    "{member.id} cannot be used on an allow_public route (anonymous and non-member callers have no member id)".into(),
                ),
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
pub(super) fn shape(path: &str) -> String {
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
