use std::time::Duration;

use moka::future::Cache;

use crate::metrics;

/// In-memory cache for validated API key results.
/// Uses blake3 hash of full key as cache key and moka for lock-free reads + fixed TTL.
#[derive(Clone)]
pub struct ApiKeyCache {
    inner: Cache<String, CachedValidation>,
}

/// Cached validation result (subset of ApiKeyValidation)
#[derive(Clone)]
pub struct CachedValidation {
    pub user_id: String,
}

impl ApiKeyCache {
    /// Create a new cache with given capacity and TTL in seconds.
    pub fn new(capacity: u64, ttl_secs: u64) -> Self {
        let cache = Cache::builder()
            .max_capacity(capacity)
            .time_to_live(Duration::from_secs(ttl_secs))
            .build();
        Self { inner: cache }
    }

    /// Get cached validation for key if present and not expired.
    pub async fn get(&self, key: &str) -> Option<CachedValidation> {
        let hash = hash_key(key);
        let result = self.inner.get(&hash).await;
        if result.is_some() {
            metrics::record_auth_cache_hit("apikey");
        } else {
            metrics::record_auth_cache_miss("apikey");
        }
        result
    }

    /// Cache a valid API key validation result.
    pub async fn put(&self, key: &str, user_id: String) {
        let hash = hash_key(key);
        self.inner.insert(hash, CachedValidation { user_id }).await;
    }
}

fn hash_key(key: &str) -> String {
    blake3::hash(key.as_bytes()).to_hex().to_string()
}
