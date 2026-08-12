use crate::auth::cache::{member_resolve_cache_key, TtlCache};

/// Cache for member resolve admissions.
/// Key: user_id + organization_id. Value: member_id (active only).
#[derive(Clone)]
pub struct MemberCache {
    inner: TtlCache<String>,
}

impl MemberCache {
    pub fn new(capacity: u64, ttl_secs: u64) -> Self {
        Self {
            inner: TtlCache::new(capacity, ttl_secs, "member"),
        }
    }

    pub async fn get(&self, user_id: &str, organization_id: &str) -> Option<String> {
        self.inner
            .get(&member_resolve_cache_key(user_id, organization_id))
            .await
    }

    pub async fn put(&self, user_id: &str, organization_id: &str, member_id: String) {
        self.inner
            .put(
                member_resolve_cache_key(user_id, organization_id),
                member_id,
            )
            .await;
    }
}
