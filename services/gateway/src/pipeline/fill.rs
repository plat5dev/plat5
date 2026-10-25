//! A route's upstream template: compiled when the route is built, filled per
//! request (docs/reference/config/services.md#upstream).

use std::collections::BTreeMap;

use crate::route_map::Scope;

#[derive(Debug, PartialEq, Eq)]
enum Part {
    Text(String),
    /// `{<kind>.id}`: the chain node at this position.
    Node(usize),
    /// `{member.id}`
    Member,
    /// `{user.id}`
    User,
    /// `{<param>}`: one of the route's own params, as matched.
    Param(String),
}

#[derive(Debug, PartialEq, Eq)]
pub struct Upstream {
    parts: Vec<Part>,
}

/// What the gateway fills an upstream with. Never a slug, never a principal id.
#[derive(Clone, Copy)]
pub struct Values<'a> {
    /// The chain's node ids, root first.
    pub nodes: &'a [&'a str],
    pub member_id: Option<&'a str>,
    pub user_id: Option<&'a str>,
    pub params: &'a BTreeMap<String, String>,
}

/// A route param that would change the path the upstream sees: **400**.
#[derive(Debug, PartialEq, Eq)]
pub struct BadParam;

impl Upstream {
    /// Err names a placeholder the scope doesn't provide.
    pub fn compile(
        template: &str,
        scope: &Scope,
        chain: &[String],
        own_params: &[&str],
    ) -> Result<Self, String> {
        let mut parts = Vec::new();
        let mut rest = template;
        while let Some(start) = rest.find('{') {
            if start > 0 {
                parts.push(Part::Text(rest[..start].to_string()));
            }
            let after = &rest[start + 1..];
            let end = after
                .find('}')
                .ok_or_else(|| "unclosed { in upstream".to_string())?;
            let name = &after[..end];
            let part = match name.split_once('.') {
                Some(("member", "id")) if matches!(scope, Scope::Kind(_)) => Some(Part::Member),
                Some(("user", "id")) if *scope == Scope::User => Some(Part::User),
                Some((kind, "id")) => chain.iter().position(|k| k == kind).map(Part::Node),
                Some(_) => None,
                None => own_params
                    .contains(&name)
                    .then(|| Part::Param(name.to_string())),
            };
            parts.push(part.ok_or_else(|| format!("upstream placeholder {{{name}}}"))?);
            rest = &after[end + 1..];
        }
        if !rest.is_empty() {
            parts.push(Part::Text(rest.to_string()));
        }
        Ok(Self { parts })
    }

    /// The upstream path. Inserted values are not scanned again.
    pub fn fill(&self, values: Values<'_>) -> Result<String, BadParam> {
        let mut out = String::new();
        for part in &self.parts {
            match part {
                Part::Text(text) => out.push_str(text),
                Part::Node(i) => out.push_str(values.nodes[*i]),
                Part::Member => out.push_str(values.member_id.unwrap_or_default()),
                Part::User => out.push_str(values.user_id.unwrap_or_default()),
                Part::Param(name) => {
                    let value = values.params.get(name).ok_or(BadParam)?;
                    if !stays_one_segment(value) {
                        return Err(BadParam);
                    }
                    out.push_str(value);
                }
            }
        }
        Ok(out)
    }
}

/// A param value, decoded, that is not `.` or `..` and holds no `/`, `?` or `#`.
fn stays_one_segment(value: &str) -> bool {
    let decoded = percent_decode(value.as_bytes());
    decoded != b"." && decoded != b".." && !decoded.iter().any(|b| b"/?#".contains(b))
}

fn percent_decode(raw: &[u8]) -> Vec<u8> {
    let hex = |b: u8| (b as char).to_digit(16).map(|d| d as u8);
    let mut out = Vec::with_capacity(raw.len());
    let mut i = 0;
    while i < raw.len() {
        let decoded = match raw.get(i..i + 3) {
            Some([b'%', h, l]) => hex(*h).zip(hex(*l)).map(|(h, l)| h << 4 | l),
            _ => None,
        };
        match decoded {
            Some(b) => {
                out.push(b);
                i += 3;
            }
            None => {
                out.push(raw[i]);
                i += 1;
            }
        }
    }
    out
}

pub fn path_and_query(path: &str, query: Option<&str>) -> String {
    match query {
        Some(q) if !q.is_empty() => format!("{path}?{q}"),
        _ => path.to_string(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn chain() -> Vec<String> {
        vec!["organization".into(), "project".into()]
    }

    fn project() -> Scope {
        Scope::Kind("project".into())
    }

    fn params(pairs: &[(&str, &str)]) -> BTreeMap<String, String> {
        pairs
            .iter()
            .map(|(k, v)| ((*k).to_string(), (*v).to_string()))
            .collect()
    }

    #[test]
    fn fills_node_ids_the_member_and_params() {
        let up = Upstream::compile(
            "/projects/{project.id}/plans/{plan_id}/by/{member.id}/in/{organization.id}",
            &project(),
            &chain(),
            &["plan_id"],
        )
        .unwrap();
        let p = params(&[("plan_id", "pro")]);
        let got = up
            .fill(Values {
                nodes: &["01o", "01p"],
                member_id: Some("01m"),
                user_id: None,
                params: &p,
            })
            .unwrap();
        assert_eq!(got, "/projects/01p/plans/pro/by/01m/in/01o");
    }

    #[test]
    fn a_user_route_fills_the_user() {
        let up = Upstream::compile("/users/{user.id}/keys", &Scope::User, &[], &[]).unwrap();
        let p = params(&[]);
        let got = up
            .fill(Values {
                nodes: &[],
                member_id: None,
                user_id: Some("01u"),
                params: &p,
            })
            .unwrap();
        assert_eq!(got, "/users/01u/keys");
    }

    #[test]
    fn a_placeholder_the_scope_lacks_cannot_compile() {
        for (template, scope) in [
            ("/x/{folder.id}", project()),
            ("/x/{project.slug}", project()),
            ("/x/{user.id}", project()),
            ("/x/{member.id}", Scope::User),
            ("/x/{member.id}", Scope::Public),
            ("/x/{project}", project()),
            ("/x/{other}", project()),
            ("/x/{plan_id", project()),
        ] {
            let err = Upstream::compile(template, &scope, &chain(), &["plan_id"]).unwrap_err();
            assert!(err.contains("upstream"), "{template}: {err}");
        }
    }

    #[test]
    fn a_param_that_changes_the_path_is_refused() {
        let up = Upstream::compile("/w/{id}", &Scope::Public, &[], &["id"]).unwrap();
        let fill = |v: &str| {
            let p = params(&[("id", v)]);
            up.fill(Values {
                nodes: &[],
                member_id: None,
                user_id: None,
                params: &p,
            })
        };
        for bad in [".", "..", "%2e%2E", "%2E", "a%2Fb", "a%3fb", "a%23b"] {
            assert_eq!(fill(bad), Err(BadParam), "{bad}");
        }
        for ok in ["a.b", "...", "%41", "a%2", "%zz", "a%20b"] {
            assert_eq!(fill(ok), Ok(format!("/w/{ok}")), "{ok}");
        }
    }

    #[test]
    fn preserves_query() {
        assert_eq!(
            path_and_query("/users/01u/nodes", Some("kind=project")),
            "/users/01u/nodes?kind=project"
        );
        assert_eq!(path_and_query("/users/01u", None), "/users/01u");
    }
}
