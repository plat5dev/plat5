use serde::{Deserialize, Serialize};
use tracing::info;

use crate::auth::call::AuthCallTimer;
use crate::auth::AuthType;
use crate::internal_http::InternalHttpClient;

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
        let timer = AuthCallTimer::start(AuthType::UserApiKey.as_str());

        let result = self
            .http
            .post_json::<_, UserApiKeyValidation>(&self.validate_url, &ValidateRequest { key })
            .await;

        let validation = match result {
            Ok(v) => v,
            Err(err) => {
                return Err(UserApiKeyError::ServiceError(
                    timer.finish_transport(err, "user key validate"),
                ));
            }
        };

        if !validation.valid {
            timer.finish("invalid");
            return Err(UserApiKeyError::InvalidKey);
        }

        timer.finish("ok");
        Ok(validation)
    }
}
