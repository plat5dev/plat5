use std::time::{Duration, SystemTime, UNIX_EPOCH};

use jsonwebtoken::TokenData;
use moka::future::Cache;
use serde_json::Value;

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

    #[cfg(test)]
    pub(crate) fn with_fixed_ttl(capacity: u64, ttl: Duration) -> Self {
        let cache = Cache::builder()
            .max_capacity(capacity)
            .time_to_live(ttl)
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

    /// Evict a cached token immediately.
    pub async fn invalidate(&self, token: &str) {
        self.inner.invalidate(&hash_secret(token)).await;
    }
}

/// Parse `exp` (NumericDate) from integer or finite non-negative float seconds.
pub fn parse_exp(claims: &Value) -> Option<u64> {
    let raw = claims.get("exp")?;
    if let Some(secs) = raw.as_u64() {
        return Some(secs);
    }
    let f = raw.as_f64()?;
    if !f.is_finite() || f < 0.0 || f >= u64::MAX as f64 {
        return None;
    }
    Some(f.trunc() as u64)
}

/// True only when `claims.exp` parses and is strictly in the future.
pub fn is_unexpired(claims: &Value) -> bool {
    let Some(now_secs) = now_unix_secs() else {
        return false;
    };
    parse_exp(claims).is_some_and(|exp| exp > now_secs)
}

fn now_unix_secs() -> Option<u64> {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .ok()
        .map(|d| d.as_secs())
}

fn ttl_for_claims(claims: &Value, now_secs: u64, ttl_buffer: Duration) -> Duration {
    let Some(exp) = parse_exp(claims) else {
        return Duration::ZERO;
    };
    if exp <= now_secs {
        return Duration::ZERO;
    }
    Duration::from_secs(
        exp.saturating_sub(now_secs)
            .saturating_sub(ttl_buffer.as_secs()),
    )
}

fn hash_secret(secret: &str) -> String {
    blake3::hash(secret.as_bytes()).to_hex().to_string()
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
        let Some(now_secs) = now_unix_secs() else {
            return Some(Duration::ZERO);
        };
        Some(ttl_for_claims(&value.claims, now_secs, self.ttl_buffer))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use jsonwebtoken::{Algorithm, Header};
    use moka::Expiry;
    use serde_json::json;

    fn token_data(claims: Value) -> TokenData<Value> {
        TokenData {
            header: Header::new(Algorithm::EdDSA),
            claims,
        }
    }

    #[test]
    fn parse_exp_handles_integers_and_floats_and_rejects_bad_values() {
        assert_eq!(
            parse_exp(&json!({ "exp": 1_700_000_000u64 })),
            Some(1_700_000_000)
        );
        assert_eq!(
            parse_exp(&json!({ "exp": 1_700_000_000.0 })),
            Some(1_700_000_000)
        );
        assert_eq!(
            parse_exp(&json!({ "exp": 1_700_000_000.9 })),
            Some(1_700_000_000)
        );
        assert_eq!(parse_exp(&json!({ "exp": 0.0 })), Some(0));
        assert_eq!(parse_exp(&json!({})), None);
        assert_eq!(parse_exp(&json!({ "exp": -1 })), None);
        assert_eq!(parse_exp(&json!({ "exp": -0.5 })), None);
        assert_eq!(parse_exp(&json!({ "exp": "1700000000" })), None);
        assert_eq!(parse_exp(&json!({ "exp": 1e30 })), None);
    }

    #[test]
    fn jwt_expiry_fails_closed_for_missing_unparseable_and_expired_tokens() {
        let expiry = JwtExpiry {
            ttl_buffer: Duration::from_secs(60),
        };
        let now = std::time::Instant::now();
        let now_secs = now_unix_secs().unwrap();
        let key = "k".to_string();

        for bad in [
            json!({}),
            json!({ "exp": "nope" }),
            json!({ "exp": -10.5 }),
            json!({ "exp": now_secs }),
            json!({ "exp": now_secs.saturating_sub(5) }),
            json!({ "exp": (now_secs.saturating_sub(5)) as f64 }),
        ] {
            assert_eq!(
                expiry.expire_after_create(&key, &token_data(bad.clone()), now),
                Some(Duration::ZERO),
                "must fail closed to Duration::ZERO, never None or 1s: {bad}"
            );
        }

        // Float exp in the future gets a bounded finite TTL (never None).
        let future_float = json!({ "exp": (now_secs + 120) as f64 + 0.5 });
        let ttl = expiry
            .expire_after_create(&key, &token_data(future_float), now)
            .expect("must always return Some(ttl)");
        assert!(
            ttl > Duration::ZERO && ttl <= Duration::from_secs(60),
            "{ttl:?}"
        );
    }

    #[tokio::test]
    async fn cache_caches_future_float_exp_and_drops_expired_immediately() {
        let cache = JwtCache::new(100, 0);
        let now_secs = now_unix_secs().unwrap();

        let valid = token_data(json!({ "sub": "u1", "exp": (now_secs + 120) as f64 }));
        cache.put("valid.jwt.token", valid.clone()).await;
        assert!(cache.get("valid.jwt.token").await.is_some());

        let expired =
            token_data(json!({ "sub": "u2", "exp": (now_secs.saturating_sub(1)) as f64 }));
        cache.put("expired.jwt.token", expired).await;
        assert!(
            cache.get("expired.jwt.token").await.is_none(),
            "already-expired token must not stay cached"
        );
    }
}
