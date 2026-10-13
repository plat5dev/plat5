use serde::{Deserialize, Serialize};
use tracing::{info, warn};

use crate::auth::call::AuthCallTimer;
use crate::internal_http::InternalHttpClient;

/// Identity's one validate: `POST /internal/credentials/validate`
/// (docs/identity.md#credential-validate). Every `X-API-Key`, and every verified
/// JWT's issuer and subject.
#[derive(Clone)]
pub struct CredentialValidator {
    validate_url: String,
    http: InternalHttpClient,
}

#[derive(Serialize)]
#[serde(rename_all = "snake_case")]
enum ValidateRequest<'a> {
    Credential(&'a str),
    Idp { issuer: &'a str, subject: &'a str },
}

#[derive(Debug, Clone, Deserialize)]
struct Validation {
    valid: bool,
    realm: Option<String>,
    kind: Option<String>,
    user_id: Option<String>,
    member_id: Option<String>,
    organization_id: Option<String>,
    labels: Option<Vec<String>>,
}

/// Whose a valid credential is. A directory result names the person; an
/// organization result names the member and its role labels, never the person.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Validated {
    Directory {
        user_id: String,
    },
    Organization {
        member_id: String,
        organization_id: String,
        /// The member's role labels: `["*"]` is every label, `[]` is none.
        labels: Vec<String>,
        session: bool,
    },
    Invalid,
}

#[derive(Debug)]
pub struct ValidateError(pub String);

impl CredentialValidator {
    /// `validate_url` e.g. http://identity:3001/internal/credentials/validate
    pub fn new(validate_url: String, http: InternalHttpClient) -> Self {
        info!(validate_url = %validate_url, "credential validator initialized");
        Self { validate_url, http }
    }

    /// An `X-API-Key` value. `metric` names the call in `auth_validation_*`.
    pub async fn credential(
        &self,
        value: &str,
        metric: &'static str,
    ) -> Result<Validated, ValidateError> {
        self.call(
            &ValidateRequest::Credential(value),
            metric,
            "credential validate",
        )
        .await
    }

    /// A verified JWT's issuer and subject, resolved to the person. A new subject
    /// creates the person; a suspended person is `Invalid`.
    pub async fn idp(&self, issuer: &str, subject: &str) -> Result<Validated, ValidateError> {
        self.call(
            &ValidateRequest::Idp { issuer, subject },
            "idp",
            "idp subject resolve",
        )
        .await
    }

    async fn call(
        &self,
        req: &ValidateRequest<'_>,
        metric: &'static str,
        op: &str,
    ) -> Result<Validated, ValidateError> {
        let timer = AuthCallTimer::start(metric);
        let v = match self
            .http
            .post_json::<_, Validation>(&self.validate_url, req)
            .await
        {
            Ok(v) => v,
            Err(err) => return Err(ValidateError(timer.finish_transport(err, op))),
        };
        if !v.valid {
            timer.finish("invalid");
            return Ok(Validated::Invalid);
        }
        match project(v) {
            Ok(validated) => {
                timer.finish("ok");
                Ok(validated)
            }
            Err(missing) => {
                timer.finish("error");
                warn!(missing, "{op} returned valid without a field it needs");
                Err(ValidateError(format!("missing {missing}")))
            }
        }
    }
}

/// A valid result missing a field is identity's bug: 503, never admit.
fn project(v: Validation) -> Result<Validated, &'static str> {
    fn need(field: Option<String>, name: &'static str) -> Result<String, &'static str> {
        field.filter(|s| !s.is_empty()).ok_or(name)
    }
    match v.realm.as_deref() {
        Some("directory") => Ok(Validated::Directory {
            user_id: need(v.user_id, "user_id")?,
        }),
        Some("organization") => Ok(Validated::Organization {
            member_id: need(v.member_id, "member_id")?,
            organization_id: need(v.organization_id, "organization_id")?,
            labels: v.labels.ok_or("labels")?,
            session: v.kind.as_deref() == Some("session"),
        }),
        _ => Err("realm"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn validation(json: &str) -> Validation {
        serde_json::from_str(json).unwrap()
    }

    #[test]
    fn request_bodies() {
        assert_eq!(
            serde_json::to_string(&ValidateRequest::Credential("plat5-k-2-x")).unwrap(),
            r#"{"credential":"plat5-k-2-x"}"#
        );
        assert_eq!(
            serde_json::to_string(&ValidateRequest::Idp {
                issuer: "https://auth",
                subject: "s"
            })
            .unwrap(),
            r#"{"idp":{"issuer":"https://auth","subject":"s"}}"#
        );
    }

    #[test]
    fn projects_each_realm() {
        assert_eq!(
            project(validation(
                r#"{"valid":true,"realm":"directory","kind":"idp","user_id":"u1"}"#
            )),
            Ok(Validated::Directory {
                user_id: "u1".into()
            })
        );
        assert_eq!(
            project(validation(
                r#"{"valid":true,"realm":"organization","kind":"session","member_id":"m","organization_id":"o","labels":[]}"#
            )),
            Ok(Validated::Organization {
                member_id: "m".into(),
                organization_id: "o".into(),
                labels: vec![],
                session: true
            })
        );
    }

    #[test]
    fn a_valid_result_missing_a_field_is_an_error() {
        for (json, missing) in [
            (r#"{"valid":true,"realm":"directory"}"#, "user_id"),
            (
                r#"{"valid":true,"realm":"organization","member_id":"m","organization_id":"o"}"#,
                "labels",
            ),
            (
                r#"{"valid":true,"realm":"organization","organization_id":"o","labels":[]}"#,
                "member_id",
            ),
            (r#"{"valid":true,"user_id":"u"}"#, "realm"),
        ] {
            assert_eq!(project(validation(json)), Err(missing), "{json}");
        }
    }
}
