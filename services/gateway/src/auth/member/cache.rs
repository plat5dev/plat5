use std::sync::Arc;

use crate::auth::cache::{member_resolve_cache_key, TtlCache};

#[derive(Clone, Debug)]
pub enum CachedMember {
    Active(String),
    Miss,
}

/// Cache for member resolve admissions.
/// Key: user_id + organization_id.
#[derive(Clone)]
pub struct MemberCache {
    inner: TtlCache<CachedMember>,
}

impl MemberCache {
    pub fn new(capacity: u64, ttl_secs: u64) -> Self {
        Self {
            inner: TtlCache::new(capacity, ttl_secs, "member"),
        }
    }

    pub async fn get_or_load<E, Fut>(
        &self,
        user_id: &str,
        organization_id: &str,
        init: Fut,
    ) -> Result<CachedMember, Arc<E>>
    where
        Fut: std::future::Future<Output = Result<CachedMember, E>>,
        E: Send + Sync + 'static,
    {
        self.inner
            .try_get_with(member_resolve_cache_key(user_id, organization_id), init)
            .await
    }
}
