use std::collections::HashMap;

use crate::auth::admit::NodeRef;

/// What the gateway sends a service: each chain node's id, and the caller's member
/// id in that tree. Never a slug, never a person's id.
#[derive(Clone, Copy)]
pub struct Subject<'a> {
    pub nodes: &'a [NodeRef],
    pub member_id: Option<&'a str>,
}

impl Subject<'_> {
    fn field(&self, name: &str) -> Option<&str> {
        if name == "member_id" {
            return self.member_id;
        }
        let kind = name.strip_suffix("_id")?;
        self.nodes
            .iter()
            .find(|n| n.kind == kind)
            .map(|n| n.id.as_str())
    }
}

#[derive(Debug, PartialEq, Eq)]
pub enum RewriteError {
    /// Client path param is not one segment.
    PathParam,
    /// Subject id is not one segment. Platform bug.
    Subject,
    /// Template the gateway's own load check should have refused.
    Internal,
}

/// Substitute `{subject.*}` and `{path.*}` in an absolute upstream template.
/// Inserted values are not scanned again.
pub fn substitute(
    template: &str,
    path_params: &HashMap<String, String>,
    subject: Subject<'_>,
) -> Result<String, RewriteError> {
    let mut out = String::with_capacity(template.len());
    let mut rest = template;
    while let Some(start) = rest.find('{') {
        out.push_str(&rest[..start]);
        let after = &rest[start + 1..];
        let Some(end) = after.find('}') else {
            return Err(RewriteError::Internal);
        };
        let body = &after[..end];
        let Some((namespace, name)) = body.split_once('.') else {
            return Err(RewriteError::Internal);
        };
        let value = match namespace {
            "subject" => subject.field(name).ok_or(RewriteError::Internal)?,
            "path" => path_params
                .get(name)
                .map(String::as_str)
                .ok_or(RewriteError::Internal)?,
            _ => return Err(RewriteError::Internal),
        };
        if !one_segment(value) {
            return Err(match namespace {
                "path" => RewriteError::PathParam,
                _ => RewriteError::Subject,
            });
        }
        out.push_str(value);
        rest = &after[end + 1..];
    }
    out.push_str(rest);
    Ok(out)
}

fn one_segment(value: &str) -> bool {
    !value.is_empty() && !value.contains('/') && !value.contains('?') && !value.contains('#')
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

    fn params(pairs: &[(&str, &str)]) -> HashMap<String, String> {
        pairs
            .iter()
            .map(|(k, v)| ((*k).to_string(), (*v).to_string()))
            .collect()
    }

    fn nodes(pairs: &[(&str, &str)]) -> Vec<NodeRef> {
        pairs
            .iter()
            .map(|(kind, id)| NodeRef {
                kind: kind.to_string(),
                id: id.to_string(),
            })
            .collect()
    }

    #[test]
    fn substitutes_subject_and_path() {
        let chain = nodes(&[("organization", "n_o"), ("project", "n_p")]);
        let got = substitute(
            "/projects/{subject.project_id}/plans/{path.key}/by/{subject.member_id}",
            &params(&[("key", "pro"), ("project", "web")]),
            Subject {
                nodes: &chain,
                member_id: Some("m_1"),
            },
        )
        .unwrap();
        assert_eq!(got, "/projects/n_p/plans/pro/by/m_1");
    }

    #[test]
    fn path_param_slash_is_client_error() {
        let err = substitute(
            "/widgets/{path.widget_id}",
            &params(&[("widget_id", "a/b")]),
            Subject {
                nodes: &[],
                member_id: None,
            },
        )
        .unwrap_err();
        assert_eq!(err, RewriteError::PathParam);
    }

    #[test]
    fn subject_slash_is_platform_error() {
        let chain = nodes(&[("organization", "n/1")]);
        let err = substitute(
            "/nodes/{subject.organization_id}",
            &params(&[]),
            Subject {
                nodes: &chain,
                member_id: None,
            },
        )
        .unwrap_err();
        assert_eq!(err, RewriteError::Subject);
    }

    #[test]
    fn a_field_the_subject_lacks_is_internal() {
        let err = substitute(
            "/nodes/{subject.folder_id}",
            &params(&[]),
            Subject {
                nodes: &[],
                member_id: Some("m_1"),
            },
        )
        .unwrap_err();
        assert_eq!(err, RewriteError::Internal);
    }

    #[test]
    fn preserves_query() {
        assert_eq!(
            path_and_query("/members/m1/nodes", Some("kind=project")),
            "/members/m1/nodes?kind=project"
        );
        assert_eq!(path_and_query("/me", None), "/me");
    }
}
