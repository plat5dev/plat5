use std::time::Duration;

use moka::future::Cache;

use crate::metrics;

/// Cache for validated member API keys (plat5-mk-1-).
#[derive(Clone)]
pub struct MemberApiKeyCache {
    inner: Cache<String, CachedMemberApiKey>,
}

#[derive(Clone)]
pub struct CachedMemberApiKey {
    pub member_id: String,
    pub organization_id: String,
}

impl MemberApiKeyCache {
    pub fn new(capacity: u64, ttl_secs: u64) -> Self {
        let cache = Cache::builder()
            .max_capacity(capacity)
            .time_to_live(Duration::from_secs(ttl_secs))
            .build();
        Self { inner: cache }
    }

    pub async fn get(&self, key: &str) -> Option<CachedMemberApiKey> {
        let hash = hash_key(key);
        let result = self.inner.get(&hash).await;
        if result.is_some() {
            metrics::record_auth_cache_hit("member_apikey");
        } else {
            metrics::record_auth_cache_miss("member_apikey");
        }
        result
    }

    pub async fn put(&self, key: &str, member_id: String, organization_id: String) {
        let hash = hash_key(key);
        self.inner
            .insert(
                hash,
                CachedMemberApiKey {
                    member_id,
                    organization_id,
                },
            )
            .await;
    }
}

fn hash_key(key: &str) -> String {
    blake3::hash(key.as_bytes()).to_hex().to_string()
}
