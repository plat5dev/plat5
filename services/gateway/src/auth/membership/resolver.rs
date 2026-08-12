use serde::{Deserialize, Serialize};
use tracing::{info, warn};

use crate::error::ErrorKind;
use crate::internal_http::{InternalHttpClient, InternalHttpError};
use crate::metrics;

/// Client for identity internal member resolve.
#[derive(Clone)]
pub struct MembershipResolver {
    resolve_url: String,
    http: InternalHttpClient,
}

#[derive(Debug, Clone, Deserialize)]
pub struct MembershipResolve {
    pub member_id: String,
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
    pub fn new(resolve_url: String, http: InternalHttpClient) -> Self {
        info!(resolve_url = %resolve_url, "member resolver initialized");
        Self { resolve_url, http }
    }

    pub async fn resolve(
        &self,
        user_id: &str,
        organization_id: &str,
    ) -> Result<MembershipResolve, MembershipError> {
        let start = std::time::Instant::now();

        let result = self
            .http
            .post_json::<_, MembershipResolve>(
                &self.resolve_url,
                &ResolveRequest {
                    user_id,
                    organization_id,
                },
            )
            .await;

        let duration = start.elapsed().as_secs_f64();

        match result {
            Ok(resolved) => {
                metrics::record_auth_validation("membership", "ok", duration);
                Ok(resolved)
            }
            Err(err) if err.is_not_found() => {
                metrics::record_auth_validation("membership", "not_found", duration);
                Err(MembershipError::NotFound)
            }
            Err(InternalHttpError::Network(msg)) => {
                metrics::record_auth_validation("membership", "error", duration);
                warn!(
                    error_kind = ErrorKind::Network.as_str(),
                    error_message = %msg,
                    "failed to call member resolve"
                );
                Err(MembershipError::ServiceError(msg))
            }
            Err(InternalHttpError::HttpStatus { status }) => {
                metrics::record_auth_validation("membership", "error", duration);
                warn!(
                    error_kind = ErrorKind::Network.as_str(),
                    status,
                    "member resolve returned error status"
                );
                Err(MembershipError::ServiceError(format!(
                    "member resolve returned status {status}"
                )))
            }
            Err(InternalHttpError::Decode(msg)) => {
                metrics::record_auth_validation("membership", "error", duration);
                warn!(
                    error_kind = ErrorKind::Internal.as_str(),
                    error_message = %msg,
                    "failed to parse member resolve response"
                );
                Err(MembershipError::ServiceError(msg))
            }
        }
    }
}
