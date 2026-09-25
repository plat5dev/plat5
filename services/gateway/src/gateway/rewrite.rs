use std::collections::HashMap;

/// Subject ids the route scope is allowed to see. Dropped ids are absent.
#[derive(Clone, Copy)]
pub struct SubjectRef<'a> {
    pub user_id: Option<&'a str>,
    pub organization_id: Option<&'a str>,
    pub member_id: Option<&'a str>,
}

#[derive(Debug)]
pub enum RewriteError {
    /// Client path param is not one segment.
    PathParam,
    /// Subject id is not one segment. Platform bug.
    Subject,
    /// Template or URI the apply-time check should have rejected.
    Internal,
}

/// Substitute `{subject.*}` and `{path.*}` in an absolute upstream template.
/// Inserted values are not scanned again.
pub fn substitute(
    template: &str,
    path_params: &HashMap<String, String>,
    subject: SubjectRef<'_>,
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
            "subject" => subject_value(name, subject).ok_or(RewriteError::Internal)?,
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

fn subject_value<'a>(name: &str, subject: SubjectRef<'a>) -> Option<&'a str> {
    match name {
        "user_id" => subject.user_id,
        "organization_id" => subject.organization_id,
        "member_id" => subject.member_id,
        _ => None,
    }
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

    #[test]
    fn substitutes_subject_and_path() {
        let got = substitute(
            "/users/{subject.user_id}/organizations/{path.organization_id}/session",
            &params(&[("organization_id", "org_1")]),
            SubjectRef {
                user_id: Some("user_1"),
                organization_id: None,
                member_id: None,
            },
        )
        .unwrap();
        assert_eq!(got, "/users/user_1/organizations/org_1/session");
    }

    #[test]
    fn path_param_slash_is_client_error() {
        let err = substitute(
            "/widgets/{path.widget_id}",
            &params(&[("widget_id", "a/b")]),
            SubjectRef {
                user_id: None,
                organization_id: None,
                member_id: None,
            },
        )
        .unwrap_err();
        assert!(matches!(err, RewriteError::PathParam));
    }

    #[test]
    fn subject_slash_is_platform_error() {
        let err = substitute(
            "/organizations/{subject.organization_id}",
            &params(&[]),
            SubjectRef {
                user_id: None,
                organization_id: Some("org/1"),
                member_id: None,
            },
        )
        .unwrap_err();
        assert!(matches!(err, RewriteError::Subject));
    }

    #[test]
    fn preserves_query() {
        assert_eq!(
            path_and_query("/users/u1/memberships", Some("limit=10")),
            "/users/u1/memberships?limit=10"
        );
        assert_eq!(path_and_query("/org", None), "/org");
    }
}
