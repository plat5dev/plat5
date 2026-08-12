use std::time::Duration;

use moka::future::Cache;

use crate::metrics;

/// Cache for member resolve admissions.
/// Key: user_id + organization_id. Value: member_id (active only).
#[derive(Clone)]
pub struct MembershipCache {
    inner: Cache<String, CachedMembership>,
}

#[derive(Clone)]
pub struct CachedMembership {
    pub member_id: String,
    pub organization_id: String,
}

impl MembershipCache {
    pub fn new(capacity: u64, ttl_secs: u64) -> Self {
        let cache = Cache::builder()
            .max_capacity(capacity)
            .time_to_live(Duration::from_secs(ttl_secs))
            .build();
        Self { inner: cache }
    }

    pub async fn get(&self, user_id: &str, organization_id: &str) -> Option<CachedMembership> {
        let key = cache_key(user_id, organization_id);
        let result = self.inner.get(&key).await;
        if result.is_some() {
            metrics::record_auth_cache_hit("membership");
        } else {
            metrics::record_auth_cache_miss("membership");
        }
        result
    }

    pub async fn put(&self, user_id: &str, organization_id: &str, member_id: String) {
        let key = cache_key(user_id, organization_id);
        self.inner
            .insert(
                key,
                CachedMembership {
                    member_id,
                    organization_id: organization_id.to_string(),
                },
            )
            .await;
    }
}

fn cache_key(user_id: &str, organization_id: &str) -> String {
    format!("{}\0{}", user_id, organization_id)
}
