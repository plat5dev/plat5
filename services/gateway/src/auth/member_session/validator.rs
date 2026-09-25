use serde::{Deserialize, Serialize};
use tracing::info;

use crate::auth::call::AuthCallTimer;
use crate::auth::AuthType;
use crate::internal_http::InternalHttpClient;

/// Validates member sessions via identity POST /internal/member-sessions/validate.
#[derive(Clone)]
pub struct MemberSessionValidator {
    validate_url: String,
    http: InternalHttpClient,
}

/// Response from identity POST /internal/member-sessions/validate.
#[derive(Debug, Clone, Deserialize)]
pub struct MemberSessionValidation {
    pub valid: bool,
    pub member_id: Option<String>,
    pub organization_id: Option<String>,
    /// Always null on a hit. None skips `required_scopes`, same as a JWT.
    #[serde(default)]
    pub scopes: Option<Vec<String>>,
}

#[derive(Serialize)]
struct ValidateRequest<'a> {
    token: &'a str,
}

#[derive(Debug)]
pub enum MemberSessionError {
    InvalidToken,
    ServiceError(String),
}

impl MemberSessionValidator {
    /// `validate_url` e.g. http://identity:3001/internal/member-sessions/validate
    pub fn new(validate_url: String, http: InternalHttpClient) -> Self {
        info!(validate_url = %validate_url, "member session validator initialized");
        Self { validate_url, http }
    }

    pub async fn validate(
        &self,
        token: &str,
    ) -> Result<MemberSessionValidation, MemberSessionError> {
        let timer = AuthCallTimer::start(AuthType::MemberSession.as_str());

        let result = self
            .http
            .post_json::<_, MemberSessionValidation>(&self.validate_url, &ValidateRequest { token })
            .await;

        let validation = match result {
            Ok(v) => v,
            Err(err) => {
                return Err(MemberSessionError::ServiceError(
                    timer.finish_transport(err, "member session validate"),
                ));
            }
        };

        if !validation.valid {
            timer.finish("invalid");
            return Err(MemberSessionError::InvalidToken);
        }

        timer.finish("ok");
        Ok(validation)
    }
}
