use std::sync::Arc;
use std::time::Duration;

use moka::future::Cache;

use crate::metrics;

/// Fixed-TTL cache with hit/miss metrics. Keys are opaque strings (pre-hashed secrets or composite ids).
#[derive(Clone)]
pub struct TtlCache<V: Clone + Send + Sync + 'static> {
    inner: Cache<String, V>,
    metric: &'static str,
}

impl<V: Clone + Send + Sync + 'static> TtlCache<V> {
    pub fn new(capacity: u64, ttl_secs: u64, metric: &'static str) -> Self {
        let inner = Cache::builder()
            .max_capacity(capacity)
            .time_to_live(Duration::from_secs(ttl_secs))
            .build();
        Self { inner, metric }
    }

    pub async fn get(&self, cache_key: &str) -> Option<V> {
        let result = self.inner.get(cache_key).await;
        if result.is_some() {
            metrics::record_auth_cache_hit(self.metric);
        } else {
            metrics::record_auth_cache_miss(self.metric);
        }
        result
    }

    pub async fn put(&self, cache_key: String, value: V) {
        self.inner.insert(cache_key, value).await;
    }

    pub async fn get_secret(&self, secret: &str) -> Option<V> {
        self.get(&hash_secret(secret)).await
    }

    pub async fn put_secret(&self, secret: &str, value: V) {
        self.put(hash_secret(secret), value).await;
    }

    /// Coalesce concurrent loads. `Err` is not cached.
    pub async fn try_get_with<E, Fut>(&self, cache_key: String, init: Fut) -> Result<V, Arc<E>>
    where
        Fut: std::future::Future<Output = Result<V, E>>,
        E: Send + Sync + 'static,
    {
        if let Some(v) = self.inner.get(&cache_key).await {
            metrics::record_auth_cache_hit(self.metric);
            return Ok(v);
        }
        metrics::record_auth_cache_miss(self.metric);
        self.inner.try_get_with(cache_key, init).await
    }

    pub async fn try_get_with_secret<E, Fut>(&self, secret: &str, init: Fut) -> Result<V, Arc<E>>
    where
        Fut: std::future::Future<Output = Result<V, E>>,
        E: Send + Sync + 'static,
    {
        self.try_get_with(hash_secret(secret), init).await
    }
}

pub fn hash_secret(secret: &str) -> String {
    blake3::hash(secret.as_bytes()).to_hex().to_string()
}

pub fn member_resolve_cache_key(user_id: &str, organization_id: &str) -> String {
    format!("{user_id}\0{organization_id}")
}
