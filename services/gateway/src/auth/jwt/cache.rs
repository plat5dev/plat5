use std::time::Duration;

use jsonwebtoken::TokenData;
use moka::future::Cache;
use serde_json::Value;

use crate::auth::cache::hash_secret;
use crate::auth::AuthType;
use crate::metrics;

/// In-memory cache for validated JWT claims.
/// Uses blake3 hash of full token as key and moka for lock-free reads + per-entry TTL from `exp`.
#[derive(Clone)]
pub struct JwtCache {
    inner: Cache<String, TokenData<Value>>,
}

impl JwtCache {
    /// Create a new cache with given capacity and TTL buffer.
    /// TTL buffer is subtracted from token exp to invalidate cache before token expires.
    pub fn new(capacity: u64, ttl_buffer_secs: u64) -> Self {
        let cache = Cache::builder()
            .max_capacity(capacity)
            .expire_after(JwtExpiry {
                ttl_buffer: Duration::from_secs(ttl_buffer_secs),
            })
            .build();
        Self { inner: cache }
    }

    /// Get cached claims for token if present and not expired.
    pub async fn get(&self, token: &str) -> Option<TokenData<Value>> {
        let key = hash_secret(token);
        let result = self.inner.get(&key).await;
        if result.is_some() {
            metrics::record_auth_cache_hit(AuthType::Jwt.as_str());
        } else {
            metrics::record_auth_cache_miss(AuthType::Jwt.as_str());
        }
        result
    }

    /// Cache validated claims with TTL derived from token exp claim.
    pub async fn put(&self, token: &str, claims: TokenData<Value>) {
        self.inner.insert(hash_secret(token), claims).await;
    }
}

#[derive(Clone)]
struct JwtExpiry {
    ttl_buffer: Duration,
}

impl moka::Expiry<String, TokenData<Value>> for JwtExpiry {
    fn expire_after_create(
        &self,
        _key: &String,
        value: &TokenData<Value>,
        _created_at: std::time::Instant,
    ) -> Option<Duration> {
        let now_secs = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .ok()?
            .as_secs();
        let exp = value.claims.get("exp")?.as_u64()?;
        let ttl_secs = exp
            .saturating_sub(now_secs)
            .saturating_sub(self.ttl_buffer.as_secs())
            .max(1);
        Some(Duration::from_secs(ttl_secs))
    }
}
