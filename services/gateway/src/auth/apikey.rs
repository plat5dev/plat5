use serde::{Deserialize, Serialize};
use std::time::Duration;
use tracing::{info, warn};

use crate::error::ErrorKind;
use crate::metrics;

const DEFAULT_TIMEOUT_SECS: u64 = 5;
const INTERNAL_TOKEN_HEADER: &str = "X-Plat5-Internal-Token";

/// User API key wire prefix. Independent of member keys (`plat5-mk-1-`).
pub const USER_KEY_PREFIX: &str = "plat5-sk-1-";

/// Member API key wire prefix. Independent of user keys (`plat5-sk-1-`).
pub const MEMBER_KEY_PREFIX: &str = "plat5-mk-1-";

/// Validates user API keys via identity POST /internal/user-keys/validate.
#[derive(Clone)]
pub struct UserApiKeyValidator {
    validate_url: String,
    internal_token: Option<String>,
    client: reqwest::Client,
}

/// Response from identity POST /internal/user-keys/validate
#[derive(Debug, Clone, Deserialize)]
pub struct UserApiKeyValidation {
    pub valid: bool,
    pub user_id: Option<String>,
}

/// Validates member API keys via identity POST /internal/member-keys/validate.
/// Wired when organization-scope member-key admission is enabled.
#[derive(Clone)]
pub struct MemberApiKeyValidator {
    validate_url: String,
    internal_token: Option<String>,
    client: reqwest::Client,
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
pub enum ApiKeyError {
    InvalidKey,
    ServiceError(String),
}

impl UserApiKeyValidator {
    /// `validate_url` e.g. http://identity:3001/internal/user-keys/validate
    pub fn new(validate_url: String, internal_token: Option<String>) -> Self {
        let client = reqwest::Client::builder()
            .timeout(Duration::from_secs(DEFAULT_TIMEOUT_SECS))
            .build()
            .expect("failed to create reqwest client");

        info!(validate_url = %validate_url, "user api key validator initialized");

        Self {
            validate_url,
            internal_token,
            client,
        }
    }

    pub async fn validate(&self, key: &str) -> Result<UserApiKeyValidation, ApiKeyError> {
        let start = std::time::Instant::now();

        let mut req = self
            .client
            .post(&self.validate_url)
            .json(&ValidateRequest { key });
        if let Some(token) = &self.internal_token {
            req = req.header(INTERNAL_TOKEN_HEADER, token);
        }

        let response = req.send().await.map_err(|e| {
            let duration = start.elapsed().as_secs_f64();
            metrics::record_auth_validation("user_apikey", "error", duration);
            warn!(
                error_kind = ErrorKind::Network.as_str(),
                error_message = %e,
                "failed to call user key validate"
            );
            ApiKeyError::ServiceError(e.to_string())
        })?;

        if !response.status().is_success() {
            let duration = start.elapsed().as_secs_f64();
            metrics::record_auth_validation("user_apikey", "error", duration);
            let status = response.status();
            warn!(
                error_kind = ErrorKind::Network.as_str(),
                status = %status,
                "user key validate returned error status"
            );
            return Err(ApiKeyError::ServiceError(format!(
                "user key validate returned status {}",
                status
            )));
        }

        let validation: UserApiKeyValidation = response.json().await.map_err(|e| {
            let duration = start.elapsed().as_secs_f64();
            metrics::record_auth_validation("user_apikey", "error", duration);
            warn!(
                error_kind = ErrorKind::Internal.as_str(),
                error_message = %e,
                "failed to parse user key validate response"
            );
            ApiKeyError::ServiceError(e.to_string())
        })?;

        let duration = start.elapsed().as_secs_f64();

        if !validation.valid {
            metrics::record_auth_validation("user_apikey", "invalid", duration);
            return Err(ApiKeyError::InvalidKey);
        }

        metrics::record_auth_validation("user_apikey", "ok", duration);
        Ok(validation)
    }
}

impl MemberApiKeyValidator {
    /// `validate_url` e.g. http://identity:3001/internal/member-keys/validate
    pub fn new(validate_url: String, internal_token: Option<String>) -> Self {
        let client = reqwest::Client::builder()
            .timeout(Duration::from_secs(DEFAULT_TIMEOUT_SECS))
            .build()
            .expect("failed to create reqwest client");

        info!(validate_url = %validate_url, "member api key validator initialized");

        Self {
            validate_url,
            internal_token,
            client,
        }
    }

    pub async fn validate(&self, key: &str) -> Result<MemberApiKeyValidation, ApiKeyError> {
        let start = std::time::Instant::now();

        let mut req = self
            .client
            .post(&self.validate_url)
            .json(&ValidateRequest { key });
        if let Some(token) = &self.internal_token {
            req = req.header(INTERNAL_TOKEN_HEADER, token);
        }

        let response = req.send().await.map_err(|e| {
            let duration = start.elapsed().as_secs_f64();
            metrics::record_auth_validation("member_apikey", "error", duration);
            warn!(
                error_kind = ErrorKind::Network.as_str(),
                error_message = %e,
                "failed to call member key validate"
            );
            ApiKeyError::ServiceError(e.to_string())
        })?;

        if !response.status().is_success() {
            let duration = start.elapsed().as_secs_f64();
            metrics::record_auth_validation("member_apikey", "error", duration);
            let status = response.status();
            warn!(
                error_kind = ErrorKind::Network.as_str(),
                status = %status,
                "member key validate returned error status"
            );
            return Err(ApiKeyError::ServiceError(format!(
                "member key validate returned status {}",
                status
            )));
        }

        let validation: MemberApiKeyValidation = response.json().await.map_err(|e| {
            let duration = start.elapsed().as_secs_f64();
            metrics::record_auth_validation("member_apikey", "error", duration);
            warn!(
                error_kind = ErrorKind::Internal.as_str(),
                error_message = %e,
                "failed to parse member key validate response"
            );
            ApiKeyError::ServiceError(e.to_string())
        })?;

        let duration = start.elapsed().as_secs_f64();

        if !validation.valid {
            metrics::record_auth_validation("member_apikey", "invalid", duration);
            return Err(ApiKeyError::InvalidKey);
        }

        metrics::record_auth_validation("member_apikey", "ok", duration);
        Ok(validation)
    }
}
