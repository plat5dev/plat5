use serde::{Deserialize, Serialize};
use std::time::Duration;
use tracing::{info, warn};

use crate::error::ErrorKind;
use crate::metrics;

const DEFAULT_TIMEOUT_SECS: u64 = 5;
const INTERNAL_TOKEN_HEADER: &str = "X-Plat5-Internal-Token";

/// Client for organizations internal membership resolve.
#[derive(Clone)]
pub struct MembershipResolver {
    resolve_url: String,
    internal_token: Option<String>,
    client: reqwest::Client,
}

#[derive(Debug, Clone, Deserialize)]
pub struct MembershipResolve {
    pub membership_id: String,
    pub organization_id: String,
    pub user_id: String,
    pub status: String,
}

#[derive(Serialize)]
struct ResolveRequest<'a> {
    user_id: &'a str,
    organization_id: &'a str,
}

#[derive(Debug)]
pub enum MembershipError {
    /// No membership row (or removed) — gateway maps to 404
    NotFound,
    /// Timeout / 5xx / network — gateway maps to 503
    ServiceError(String),
}

impl MembershipResolver {
    /// `resolve_url` is the full resolve URL.
    /// `internal_token` is sent as X-Plat5-Internal-Token when Some.
    pub fn new(resolve_url: String, internal_token: Option<String>) -> Self {
        let client = reqwest::Client::builder()
            .timeout(Duration::from_secs(DEFAULT_TIMEOUT_SECS))
            .build()
            .expect("failed to create reqwest client");

        info!(resolve_url = %resolve_url, "membership resolver initialized");

        Self {
            resolve_url,
            internal_token,
            client,
        }
    }

    pub async fn resolve(
        &self,
        user_id: &str,
        organization_id: &str,
    ) -> Result<MembershipResolve, MembershipError> {
        let start = std::time::Instant::now();

        let mut req = self.client.post(&self.resolve_url).json(&ResolveRequest {
            user_id,
            organization_id,
        });
        if let Some(token) = &self.internal_token {
            req = req.header(INTERNAL_TOKEN_HEADER, token);
        }

        let response = req.send().await.map_err(|e| {
            let duration = start.elapsed().as_secs_f64();
            metrics::record_auth_validation("membership", "error", duration);
            warn!(
                error_kind = ErrorKind::Network.as_str(),
                error_message = %e,
                "failed to call membership resolve"
            );
            MembershipError::ServiceError(e.to_string())
        })?;

        let status = response.status();
        if status.as_u16() == 404 {
            let duration = start.elapsed().as_secs_f64();
            metrics::record_auth_validation("membership", "not_found", duration);
            return Err(MembershipError::NotFound);
        }

        if !status.is_success() {
            let duration = start.elapsed().as_secs_f64();
            metrics::record_auth_validation("membership", "error", duration);
            warn!(
                error_kind = ErrorKind::Network.as_str(),
                status = %status,
                "membership resolve returned error status"
            );
            return Err(MembershipError::ServiceError(format!(
                "membership resolve returned status {}",
                status
            )));
        }

        let resolved: MembershipResolve = response.json().await.map_err(|e| {
            let duration = start.elapsed().as_secs_f64();
            metrics::record_auth_validation("membership", "error", duration);
            warn!(
                error_kind = ErrorKind::Internal.as_str(),
                error_message = %e,
                "failed to parse membership resolve response"
            );
            MembershipError::ServiceError(e.to_string())
        })?;

        let duration = start.elapsed().as_secs_f64();
        metrics::record_auth_validation("membership", "ok", duration);
        Ok(resolved)
    }
}
