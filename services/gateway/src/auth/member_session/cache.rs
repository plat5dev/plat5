use std::sync::Arc;

use crate::auth::cache::TtlCache;
use crate::auth::AuthType;

#[derive(Clone, Debug)]
pub enum CachedMemberSession {
    Valid {
        member_id: String,
        organization_id: String,
        scopes: Option<Vec<String>>,
    },
    Invalid,
}

/// Cache for member sessions (hits and invalid tokens). Same TTL as API keys.
#[derive(Clone)]
pub struct MemberSessionCache {
    inner: TtlCache<CachedMemberSession>,
}

impl MemberSessionCache {
    pub fn new(capacity: u64, ttl_secs: u64) -> Self {
        Self {
            inner: TtlCache::new(capacity, ttl_secs, AuthType::MemberSession.as_str()),
        }
    }

    pub async fn get_or_load<E, Fut>(
        &self,
        token: &str,
        init: Fut,
    ) -> Result<CachedMemberSession, Arc<E>>
    where
        Fut: std::future::Future<Output = Result<CachedMemberSession, E>>,
        E: Send + Sync + 'static,
    {
        self.inner.try_get_with_secret(token, init).await
    }
}
