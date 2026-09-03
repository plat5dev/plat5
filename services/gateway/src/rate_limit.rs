use std::time::{SystemTime, UNIX_EPOCH};

use redis::aio::ConnectionManager;
use redis::{Client, RedisError, Script};
use tracing::warn;

const KEY_PREFIX: &str = "gw:rl:";

const ALLOW_SCRIPT: &str = r#"
local n = tonumber(redis.call('GET', KEYS[1]) or '0')
local limit = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
if n >= limit then
  local ttl = redis.call('TTL', KEYS[1])
  if ttl < 0 then ttl = window end
  return {0, n, ttl}
end
n = redis.call('INCR', KEYS[1])
if n == 1 then
  redis.call('EXPIRE', KEYS[1], window)
end
local ttl = redis.call('TTL', KEYS[1])
if ttl < 0 then ttl = window end
return {1, n, ttl}
"#;

/// Cluster-wide fixed-window limiter. Redis is required; no in-process fallback.
#[derive(Clone)]
pub struct RateLimiter {
    conn: ConnectionManager,
}

#[derive(Clone, Debug)]
pub struct RateLimitInfo {
    pub limit: u64,
    pub remaining: u64,
    pub reset_epoch: u64,
}

#[derive(Debug)]
pub enum RateLimitError {
    Exceeded {
        retry_after: u64,
        info: RateLimitInfo,
    },
    Unavailable,
}

impl RateLimiter {
    pub async fn connect(url: &str) -> Result<Self, RedisError> {
        let client = Client::open(url)?;
        let conn = ConnectionManager::new(client).await?;
        Ok(Self { conn })
    }

    pub async fn ping(&self) -> Result<(), RedisError> {
        let mut conn = self.conn.clone();
        let _: String = redis::cmd("PING").query_async(&mut conn).await?;
        Ok(())
    }

    pub async fn allow(
        &self,
        bucket: &str,
        limit: u64,
        window_secs: u64,
    ) -> Result<RateLimitInfo, RateLimitError> {
        if limit == 0 || window_secs == 0 {
            return Ok(RateLimitInfo {
                limit: 0,
                remaining: 0,
                reset_epoch: now_epoch().saturating_add(1),
            });
        }
        let key = format!("{KEY_PREFIX}{bucket}");
        let mut conn = self.conn.clone();
        let result: Result<Vec<i64>, RedisError> = Script::new(ALLOW_SCRIPT)
            .key(&key)
            .arg(limit)
            .arg(window_secs)
            .invoke_async(&mut conn)
            .await;
        match result {
            Ok(vals) if vals.len() >= 3 => {
                let allowed = vals[0] == 1;
                let count = vals[1].max(0) as u64;
                let ttl = vals[2].max(1) as u64;
                let remaining = if allowed {
                    limit.saturating_sub(count)
                } else {
                    0
                };
                let info = RateLimitInfo {
                    limit,
                    remaining,
                    reset_epoch: now_epoch().saturating_add(ttl),
                };
                if allowed {
                    Ok(info)
                } else {
                    Err(RateLimitError::Exceeded {
                        retry_after: ttl,
                        info,
                    })
                }
            }
            Ok(_) => {
                warn!("rate limit script returned unexpected value");
                Err(RateLimitError::Unavailable)
            }
            Err(err) => {
                warn!(error = %err, "rate limit redis error");
                Err(RateLimitError::Unavailable)
            }
        }
    }
}

fn now_epoch() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0)
}
