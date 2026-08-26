use crate::auth::cache::TtlCache;
use crate::auth::AuthType;

#[derive(Clone)]
pub struct CachedUserApiKey {
    pub user_id: String,
    pub scopes: Option<Vec<String>>,
}

/// In-memory cache for validated user API keys.
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

    pub async fn get(&self, key: &str) -> Option<CachedUserApiKey> {
        self.inner.get_secret(key).await
    }

    pub async fn put(&self, key: &str, user_id: String, scopes: Option<Vec<String>>) {
        self.inner
            .put_secret(key, CachedUserApiKey { user_id, scopes })
            .await;
    }
}
