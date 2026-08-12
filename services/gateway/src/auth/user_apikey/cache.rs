use std::time::Duration;

use moka::future::Cache;

use crate::metrics;

/// In-memory cache for validated user API key results.
/// Uses blake3 hash of full key as cache key and moka for lock-free reads + fixed TTL.
#[derive(Clone)]
pub struct UserApiKeyCache {
    inner: Cache<String, CachedUserApiKey>,
}

#[derive(Clone)]
pub struct CachedUserApiKey {
    pub user_id: String,
}

impl UserApiKeyCache {
    pub fn new(capacity: u64, ttl_secs: u64) -> Self {
        let cache = Cache::builder()
            .max_capacity(capacity)
            .time_to_live(Duration::from_secs(ttl_secs))
            .build();
        Self { inner: cache }
    }

    pub async fn get(&self, key: &str) -> Option<CachedUserApiKey> {
        let hash = hash_key(key);
        let result = self.inner.get(&hash).await;
        if result.is_some() {
            metrics::record_auth_cache_hit("user_apikey");
        } else {
            metrics::record_auth_cache_miss("user_apikey");
        }
        result
    }

    pub async fn put(&self, key: &str, user_id: String) {
        let hash = hash_key(key);
        self.inner
            .insert(hash, CachedUserApiKey { user_id })
            .await;
    }
}

fn hash_key(key: &str) -> String {
    blake3::hash(key.as_bytes()).to_hex().to_string()
}
