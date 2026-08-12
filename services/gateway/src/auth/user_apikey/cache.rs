use crate::auth::cache::TtlCache;
use crate::auth::AuthType;

/// In-memory cache for validated user API keys. Values are user ids.
#[derive(Clone)]
pub struct UserApiKeyCache {
    inner: TtlCache<String>,
}

impl UserApiKeyCache {
    pub fn new(capacity: u64, ttl_secs: u64) -> Self {
        Self {
            inner: TtlCache::new(capacity, ttl_secs, AuthType::UserApiKey.as_str()),
        }
    }

    pub async fn get(&self, key: &str) -> Option<String> {
        self.inner.get_secret(key).await
    }

    pub async fn put(&self, key: &str, user_id: String) {
        self.inner.put_secret(key, user_id).await;
    }
}
