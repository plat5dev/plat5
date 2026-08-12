use crate::auth::cache::TtlCache;
use crate::auth::AuthType;

#[derive(Clone)]
pub struct CachedMemberApiKey {
    pub member_id: String,
    pub organization_id: String,
}

/// Cache for validated member API keys (plat5-mk-1-).
#[derive(Clone)]
pub struct MemberApiKeyCache {
    inner: TtlCache<CachedMemberApiKey>,
}

impl MemberApiKeyCache {
    pub fn new(capacity: u64, ttl_secs: u64) -> Self {
        Self {
            inner: TtlCache::new(capacity, ttl_secs, AuthType::MemberApiKey.as_str()),
        }
    }

    pub async fn get(&self, key: &str) -> Option<CachedMemberApiKey> {
        self.inner.get_secret(key).await
    }

    pub async fn put(&self, key: &str, member_id: String, organization_id: String) {
        self.inner
            .put_secret(
                key,
                CachedMemberApiKey {
                    member_id,
                    organization_id,
                },
            )
            .await;
    }
}
