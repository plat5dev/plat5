use std::sync::Arc;

use crate::auth::cache::TtlCache;
use crate::auth::AuthType;

#[derive(Clone, Debug)]
pub enum CachedMemberApiKey {
    Valid {
        member_id: String,
        organization_id: String,
        scopes: Option<Vec<String>>,
    },
    Invalid,
}

/// Cache for member API keys (hits and invalid keys).
#[derive(Clone)]
pub struct MemberApiKeyCache {
    inner: TtlCache<CachedMemberApiKey>,
}

impl MemberApiKeyCache {
    pub fn new(capacity: u64, ttl_secs: u64) -> Self {
        Self {
            inner: TtlCache::new(capacity, ttl_secs, AuthType::MemberApiKey.as_str()),
        }
    }

    pub async fn get_or_load<E, Fut>(
        &self,
        key: &str,
        init: Fut,
    ) -> Result<CachedMemberApiKey, Arc<E>>
    where
        Fut: std::future::Future<Output = Result<CachedMemberApiKey, E>>,
        E: Send + Sync + 'static,
    {
        self.inner.try_get_with_secret(key, init).await
    }
}
