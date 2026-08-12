use serde::{Deserialize, Serialize};
use tracing::{info, warn};

use crate::error::ErrorKind;
use crate::internal_http::{InternalHttpClient, InternalHttpError};
use crate::metrics;

/// User API key wire prefix. Independent of member keys (`plat5-mk-1-`).
pub const USER_KEY_PREFIX: &str = "plat5-sk-1-";

/// Validates user API keys via identity POST /internal/user-keys/validate.
#[derive(Clone)]
pub struct UserApiKeyValidator {
    validate_url: String,
    http: InternalHttpClient,
}

/// Response from identity POST /internal/user-keys/validate
#[derive(Debug, Clone, Deserialize)]
pub struct UserApiKeyValidation {
    pub valid: bool,
    pub user_id: Option<String>,
}

#[derive(Serialize)]
struct ValidateRequest<'a> {
    key: &'a str,
}

#[derive(Debug)]
pub enum UserApiKeyError {
    InvalidKey,
    ServiceError(String),
}

impl UserApiKeyValidator {
    /// `validate_url` e.g. http://identity:3001/internal/user-keys/validate
    pub fn new(validate_url: String, http: InternalHttpClient) -> Self {
        info!(validate_url = %validate_url, "user api key validator initialized");
        Self { validate_url, http }
    }

    pub async fn validate(&self, key: &str) -> Result<UserApiKeyValidation, UserApiKeyError> {
        let start = std::time::Instant::now();

        let result = self
            .http
            .post_json::<_, UserApiKeyValidation>(
                &self.validate_url,
                &ValidateRequest { key },
            )
            .await;

        let duration = start.elapsed().as_secs_f64();

        let validation = match result {
            Ok(v) => v,
            Err(InternalHttpError::Network(msg)) => {
                metrics::record_auth_validation("user_apikey", "error", duration);
                warn!(
                    error_kind = ErrorKind::Network.as_str(),
                    error_message = %msg,
                    "failed to call user key validate"
                );
                return Err(UserApiKeyError::ServiceError(msg));
            }
            Err(InternalHttpError::HttpStatus { status }) => {
                metrics::record_auth_validation("user_apikey", "error", duration);
                warn!(
                    error_kind = ErrorKind::Network.as_str(),
                    status,
                    "user key validate returned error status"
                );
                return Err(UserApiKeyError::ServiceError(format!(
                    "user key validate returned status {status}"
                )));
            }
            Err(InternalHttpError::Decode(msg)) => {
                metrics::record_auth_validation("user_apikey", "error", duration);
                warn!(
                    error_kind = ErrorKind::Internal.as_str(),
                    error_message = %msg,
                    "failed to parse user key validate response"
                );
                return Err(UserApiKeyError::ServiceError(msg));
            }
        };

        if !validation.valid {
            metrics::record_auth_validation("user_apikey", "invalid", duration);
            return Err(UserApiKeyError::InvalidKey);
        }

        metrics::record_auth_validation("user_apikey", "ok", duration);
        Ok(validation)
    }
}
