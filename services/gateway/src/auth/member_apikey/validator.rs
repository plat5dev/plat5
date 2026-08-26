use serde::{Deserialize, Serialize};
use tracing::info;

use crate::auth::call::AuthCallTimer;
use crate::auth::AuthType;
use crate::internal_http::InternalHttpClient;

/// Validates member API keys via identity POST /internal/member-keys/validate.
#[derive(Clone)]
pub struct MemberApiKeyValidator {
    validate_url: String,
    http: InternalHttpClient,
}

/// Response from identity POST /internal/member-keys/validate
#[derive(Debug, Clone, Deserialize)]
pub struct MemberApiKeyValidation {
    pub valid: bool,
    pub member_id: Option<String>,
    pub organization_id: Option<String>,
    /// None = unrestricted (JSON null). Some([]) grants nothing.
    #[serde(default)]
    pub scopes: Option<Vec<String>>,
}

#[derive(Serialize)]
struct ValidateRequest<'a> {
    key: &'a str,
}

#[derive(Debug)]
pub enum MemberApiKeyError {
    InvalidKey,
    ServiceError(String),
}

impl MemberApiKeyValidator {
    /// `validate_url` e.g. http://identity:3001/internal/member-keys/validate
    pub fn new(validate_url: String, http: InternalHttpClient) -> Self {
        info!(validate_url = %validate_url, "member api key validator initialized");
        Self { validate_url, http }
    }

    pub async fn validate(&self, key: &str) -> Result<MemberApiKeyValidation, MemberApiKeyError> {
        let timer = AuthCallTimer::start(AuthType::MemberApiKey.as_str());

        let result = self
            .http
            .post_json::<_, MemberApiKeyValidation>(&self.validate_url, &ValidateRequest { key })
            .await;

        let validation = match result {
            Ok(v) => v,
            Err(err) => {
                return Err(MemberApiKeyError::ServiceError(
                    timer.finish_transport(err, "member key validate"),
                ));
            }
        };

        if !validation.valid {
            timer.finish("invalid");
            return Err(MemberApiKeyError::InvalidKey);
        }

        timer.finish("ok");
        Ok(validation)
    }
}
