use std::sync::Arc;

use crate::auth::cache::TtlCache;
use crate::auth::AuthType;

#[derive(Clone, Debug)]
pub enum CachedUserApiKey {
    Valid {
        user_id: String,
        scopes: Option<Vec<String>>,
    },
    Invalid,
}

/// In-memory cache for user API keys (hits and invalid keys).
#[derive(Clone)]
pub struct UserApiKeyCache {
    inner: TtlCache<CachedUserApiKey>,
}

impl UserApiKeyCache {
    pub fn new(capacity: u64, ttl_secs: u64) -> Self {
        Self {
            inner: TtlCache::new(capacity, ttl_secs, AuthType::UserApiKey.as_str()),
        }
    }

    pub async fn get_or_load<E, Fut>(
        &self,
        key: &str,
        init: Fut,
    ) -> Result<CachedUserApiKey, Arc<E>>
    where
        Fut: std::future::Future<Output = Result<CachedUserApiKey, E>>,
        E: Send + Sync + 'static,
    {
        self.inner.try_get_with_secret(key, init).await
    }
}
