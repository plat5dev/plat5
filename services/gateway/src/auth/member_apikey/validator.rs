use serde::{Deserialize, Serialize};
use tracing::{info, warn};

use crate::error::ErrorKind;
use crate::internal_http::{InternalHttpClient, InternalHttpError};
use crate::metrics;

/// Member API key wire prefix. Independent of user keys (`plat5-sk-1-`).
pub const MEMBER_KEY_PREFIX: &str = "plat5-mk-1-";

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
    pub user_id: Option<String>,
    pub service_account_id: Option<String>,
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
        let start = std::time::Instant::now();

        let result = self
            .http
            .post_json::<_, MemberApiKeyValidation>(
                &self.validate_url,
                &ValidateRequest { key },
            )
            .await;

        let duration = start.elapsed().as_secs_f64();

        let validation = match result {
            Ok(v) => v,
            Err(InternalHttpError::Network(msg)) => {
                metrics::record_auth_validation("member_apikey", "error", duration);
                warn!(
                    error_kind = ErrorKind::Network.as_str(),
                    error_message = %msg,
                    "failed to call member key validate"
                );
                return Err(MemberApiKeyError::ServiceError(msg));
            }
            Err(InternalHttpError::HttpStatus { status }) => {
                metrics::record_auth_validation("member_apikey", "error", duration);
                warn!(
                    error_kind = ErrorKind::Network.as_str(),
                    status,
                    "member key validate returned error status"
                );
                return Err(MemberApiKeyError::ServiceError(format!(
                    "member key validate returned status {status}"
                )));
            }
            Err(InternalHttpError::Decode(msg)) => {
                metrics::record_auth_validation("member_apikey", "error", duration);
                warn!(
                    error_kind = ErrorKind::Internal.as_str(),
                    error_message = %msg,
                    "failed to parse member key validate response"
                );
                return Err(MemberApiKeyError::ServiceError(msg));
            }
        };

        if !validation.valid {
            metrics::record_auth_validation("member_apikey", "invalid", duration);
            return Err(MemberApiKeyError::InvalidKey);
        }

        metrics::record_auth_validation("member_apikey", "ok", duration);
        Ok(validation)
    }
}
