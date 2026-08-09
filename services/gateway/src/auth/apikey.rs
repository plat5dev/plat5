use serde::{Deserialize, Serialize};
use std::time::Duration;
use tracing::{info, warn};

use crate::error::ErrorKind;
use crate::metrics;

const DEFAULT_TIMEOUT_SECS: u64 = 5;
const INTERNAL_TOKEN_HEADER: &str = "X-Plat5-Internal-Token";

/// API key validator that calls the api-keys service
#[derive(Clone)]
pub struct ApiKeyValidator {
    validate_url: String,
    internal_token: Option<String>,
    client: reqwest::Client,
}

/// Response from the api-keys service validation endpoint
#[derive(Debug, Clone, Deserialize)]
pub struct ApiKeyValidation {
    pub valid: bool,
    pub user_id: Option<String>,
}

/// Request body for the validation endpoint
#[derive(Serialize)]
struct ValidateRequest<'a> {
    key: &'a str,
}

#[derive(Debug)]
pub enum ApiKeyError {
    InvalidKey,
    ServiceError(String),
}

impl ApiKeyValidator {
    /// `validate_url` is the full validate URL (e.g. http://api-keys:3001/internal/keys/validate).
    /// `internal_token` is sent as X-Plat5-Internal-Token when Some.
    pub fn new(validate_url: String, internal_token: Option<String>) -> Self {
        let client = reqwest::Client::builder()
            .timeout(Duration::from_secs(DEFAULT_TIMEOUT_SECS))
            .build()
            .expect("failed to create reqwest client");

        info!(validate_url = %validate_url, "api key validator initialized");

        Self {
            validate_url,
            internal_token,
            client,
        }
    }

    /// Validate an API key against the api-keys service
    pub async fn validate(&self, key: &str) -> Result<ApiKeyValidation, ApiKeyError> {
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
            metrics::record_auth_validation("apikey", "error", duration);
            warn!(
                error_kind = ErrorKind::Network.as_str(),
                error_message = %e,
                "failed to call api-keys service"
            );
            ApiKeyError::ServiceError(e.to_string())
        })?;

        if !response.status().is_success() {
            let duration = start.elapsed().as_secs_f64();
            metrics::record_auth_validation("apikey", "error", duration);
            let status = response.status();
            warn!(
                error_kind = ErrorKind::Network.as_str(),
                status = %status,
                "api-keys service returned error status"
            );
            return Err(ApiKeyError::ServiceError(format!(
                "api-keys service returned status {}",
                status
            )));
        }

        let validation: ApiKeyValidation = response.json().await.map_err(|e| {
            let duration = start.elapsed().as_secs_f64();
            metrics::record_auth_validation("apikey", "error", duration);
            warn!(
                error_kind = ErrorKind::Internal.as_str(),
                error_message = %e,
                "failed to parse api-keys response"
            );
            ApiKeyError::ServiceError(e.to_string())
        })?;

        let duration = start.elapsed().as_secs_f64();

        if !validation.valid {
            metrics::record_auth_validation("apikey", "invalid", duration);
            return Err(ApiKeyError::InvalidKey);
        }

        metrics::record_auth_validation("apikey", "ok", duration);
        Ok(validation)
    }
}
