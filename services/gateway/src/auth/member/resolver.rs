use serde::{Deserialize, Serialize};
use tracing::info;

use crate::auth::call::AuthCallTimer;
use crate::internal_http::InternalHttpClient;

/// Client for identity internal member resolve.
#[derive(Clone)]
pub struct MemberResolver {
    resolve_url: String,
    http: InternalHttpClient,
}

#[derive(Debug, Clone, Deserialize)]
pub struct MemberResolve {
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
pub enum MemberError {
    /// No member row (or removed) — gateway maps to 404
    NotFound,
    /// Timeout / 5xx / network — gateway maps to 503
    ServiceError(String),
}

impl MemberResolver {
    /// `resolve_url` is the full resolve URL.
    pub fn new(resolve_url: String, http: InternalHttpClient) -> Self {
        info!(resolve_url = %resolve_url, "member resolver initialized");
        Self { resolve_url, http }
    }

    pub async fn resolve(
        &self,
        user_id: &str,
        organization_id: &str,
    ) -> Result<MemberResolve, MemberError> {
        let timer = AuthCallTimer::start("member");

        let result = self
            .http
            .post_json::<_, MemberResolve>(
                &self.resolve_url,
                &ResolveRequest {
                    user_id,
                    organization_id,
                },
            )
            .await;

        match result {
            Ok(resolved) => {
                timer.finish("ok");
                Ok(resolved)
            }
            Err(err) if err.is_not_found() => {
                timer.finish("not_found");
                Err(MemberError::NotFound)
            }
            Err(err) => Err(MemberError::ServiceError(
                timer.finish_transport(err, "member resolve"),
            )),
        }
    }
}
