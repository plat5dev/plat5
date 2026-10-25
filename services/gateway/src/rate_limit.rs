use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use arc_swap::ArcSwap;
use once_cell::sync::Lazy;
use redis::aio::{ConnectionManager, ConnectionManagerConfig};
use redis::{Client, RedisError, Script};
use tracing::{info, warn};

/// Every counter key starts with this, so a Valkey shared with other software is safe.
pub const KEY_PREFIX: &str = "plat5:rl:";

/// Command and connect bound. A dead socket must surface as an error inside this window.
const VALKEY_TIMEOUT: Duration = Duration::from_millis(500);

/// Cluster-wide fixed-window limiter. Valkey is required. A request's buckets are
/// checked together in one round trip and counted only if every one passes.
///
/// Clones share one connection. A timeout or dropped socket is replaced in the
/// background so a later request succeeds once Valkey answers again.
#[derive(Clone)]
pub struct RateLimiter {
    inner: Arc<Inner>,
}

struct Inner {
    client: Client,
    conn: ArcSwap<ConnectionManager>,
    reconnecting: AtomicBool,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct RateLimitInfo {
    pub limit: u64,
    pub remaining: u64,
    pub reset_epoch: u64,
}

/// One fixed-window counter: `key` under [`KEY_PREFIX`].
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Bucket {
    pub key: String,
    pub requests: u64,
    pub window_seconds: u64,
}

/// What a check of a request's buckets found, one info per bucket, in order.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Verdict {
    /// Every bucket passed, and each was counted.
    Allowed(Vec<RateLimitInfo>),
    /// At least one was full; none was counted. `retry_after` is the longest wait
    /// among the full ones.
    Exceeded {
        retry_after: u64,
        infos: Vec<RateLimitInfo>,
    },
}

/// Valkey didn't answer in time, or answered something else: **503**.
#[derive(Debug, PartialEq, Eq)]
pub struct Unavailable;

impl RateLimiter {
    pub async fn connect(url: &str) -> Result<Self, RedisError> {
        let client = Client::open(url)?;
        let conn = open_manager(client.clone()).await?;
        Ok(Self {
            inner: Arc::new(Inner {
                client,
                conn: ArcSwap::from_pointee(conn),
                reconnecting: AtomicBool::new(false),
            }),
        })
    }

    pub async fn ping(&self) -> Result<(), RedisError> {
        let mut conn = self.conn();
        let result = redis::cmd("PING").query_async(&mut conn).await;
        if let Err(ref err) = result {
            self.note_disconnect(err);
        }
        result
    }

    /// Check every bucket, and count them all only if all pass. No buckets, no trip.
    pub async fn check(&self, buckets: &[Bucket]) -> Result<Verdict, Unavailable> {
        if buckets.is_empty() {
            return Ok(Verdict::Allowed(Vec::new()));
        }
        let mut invocation = CHECK.prepare_invoke();
        for bucket in buckets {
            invocation
                .key(format!("{KEY_PREFIX}{}", bucket.key))
                .arg(bucket.requests)
                .arg(bucket.window_seconds);
        }
        let mut conn = self.conn();
        match invocation.invoke_async::<Vec<i64>>(&mut conn).await {
            Ok(vals) if vals.len() == 1 + 2 * buckets.len() => {
                Ok(verdict(buckets, vals[0] == 1, &vals[1..], now_epoch()))
            }
            Ok(_) => {
                warn!("rate limit script returned unexpected value");
                Err(Unavailable)
            }
            Err(err) => {
                warn!(error = %err, "rate limit valkey error");
                self.note_disconnect(&err);
                Err(Unavailable)
            }
        }
    }

    fn conn(&self) -> ConnectionManager {
        let guard = self.inner.conn.load();
        ConnectionManager::clone(&guard)
    }

    /// A response timeout does not replace the connection. Swap in a new one
    /// or later commands keep the dead socket.
    fn note_disconnect(&self, err: &RedisError) {
        if err.is_io_error() || err.is_unrecoverable_error() {
            self.spawn_reconnect();
        }
    }

    fn spawn_reconnect(&self) {
        if self.inner.reconnecting.swap(true, Ordering::AcqRel) {
            return;
        }
        let inner = Arc::clone(&self.inner);
        let Some(handle) = tokio::runtime::Handle::try_current().ok() else {
            inner.reconnecting.store(false, Ordering::Release);
            warn!("valkey reconnect skipped: no tokio runtime");
            return;
        };
        handle.spawn(async move {
            match open_manager(inner.client.clone()).await {
                Ok(conn) => {
                    inner.conn.store(Arc::new(conn));
                    info!("valkey reconnected");
                }
                Err(err) => {
                    warn!(error = %err, "valkey reconnect failed");
                }
            }
            inner.reconnecting.store(false, Ordering::Release);
        });
    }
}

/// `0` is one attempt. redis-rs passes this count to backon as extra sleeps
/// after the first failure.
fn manager_config() -> ConnectionManagerConfig {
    ConnectionManagerConfig::new()
        .set_response_timeout(VALKEY_TIMEOUT)
        .set_connection_timeout(VALKEY_TIMEOUT)
        .set_number_of_retries(0)
}

async fn open_manager(client: Client) -> Result<ConnectionManager, RedisError> {
    ConnectionManager::new_with_config(client, manager_config()).await
}

/// KEYS are the buckets; ARGV holds each one's limit and window, in order.
/// Returns `{allowed, count1, ttl1, count2, ttl2, ...}`: every bucket is read
/// first, and incremented only when none is full.
const CHECK_SCRIPT: &str = r#"
local allowed = 1
for i, key in ipairs(KEYS) do
  local n = tonumber(redis.call('GET', key) or '0')
  if n >= tonumber(ARGV[2 * i - 1]) then allowed = 0 end
end
local out = {allowed}
for i, key in ipairs(KEYS) do
  local window = tonumber(ARGV[2 * i])
  local n
  if allowed == 1 then
    n = redis.call('INCR', key)
    if n == 1 then redis.call('EXPIRE', key, window) end
  else
    n = tonumber(redis.call('GET', key) or '0')
  end
  local ttl = redis.call('TTL', key)
  if ttl < 0 then ttl = window end
  table.insert(out, n)
  table.insert(out, ttl)
end
return out
"#;

static CHECK: Lazy<Script> = Lazy::new(|| Script::new(CHECK_SCRIPT));

/// The verdict from the script's counts and TTLs (`counts` is `{count, ttl}` per bucket).
fn verdict(buckets: &[Bucket], allowed: bool, counts: &[i64], now: u64) -> Verdict {
    let mut infos = Vec::with_capacity(buckets.len());
    let mut retry_after = 0;
    for (bucket, pair) in buckets.iter().zip(counts.chunks(2)) {
        let count = pair[0].max(0) as u64;
        let ttl = pair[1].max(1) as u64;
        let full = !allowed && count >= bucket.requests;
        if full {
            retry_after = retry_after.max(ttl);
        }
        infos.push(RateLimitInfo {
            limit: bucket.requests,
            remaining: if full {
                0
            } else {
                bucket.requests.saturating_sub(count)
            },
            reset_epoch: now.saturating_add(ttl),
        });
    }
    if allowed {
        Verdict::Allowed(infos)
    } else {
        Verdict::Exceeded { retry_after, infos }
    }
}

fn now_epoch() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::AtomicBool;
    use std::time::Instant;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    use tokio::net::{TcpListener, TcpStream};

    fn bucket() -> Bucket {
        Bucket {
            key: "bucket".into(),
            requests: 10,
            window_seconds: 60,
        }
    }

    #[test]
    fn a_full_bucket_refuses_and_counts_nothing() {
        let buckets = [
            Bucket {
                key: "a".into(),
                requests: 3,
                window_seconds: 1,
            },
            Bucket {
                key: "b".into(),
                requests: 2,
                window_seconds: 10,
            },
        ];
        assert_eq!(
            verdict(&buckets, true, &[1, 1, 2, 9], 100),
            Verdict::Allowed(vec![
                RateLimitInfo {
                    limit: 3,
                    remaining: 2,
                    reset_epoch: 101
                },
                RateLimitInfo {
                    limit: 2,
                    remaining: 0,
                    reset_epoch: 109
                },
            ])
        );
        // b is full: nothing was counted, and a's count is as it was.
        assert_eq!(
            verdict(&buckets, false, &[1, 1, 2, 9], 100),
            Verdict::Exceeded {
                retry_after: 9,
                infos: vec![
                    RateLimitInfo {
                        limit: 3,
                        remaining: 2,
                        reset_epoch: 101
                    },
                    RateLimitInfo {
                        limit: 2,
                        remaining: 0,
                        reset_epoch: 109
                    },
                ]
            }
        );
    }

    #[tokio::test]
    async fn connect_to_silent_peer_times_out() {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move {
            loop {
                let Ok((mut sock, _)) = listener.accept().await else {
                    return;
                };
                tokio::spawn(async move {
                    let mut buf = [0u8; 64];
                    let _ = sock.read(&mut buf).await;
                    std::future::pending::<()>().await;
                });
            }
        });

        let started = Instant::now();
        let result = RateLimiter::connect(&format!("redis://{addr}")).await;
        assert!(result.is_err());
        assert!(
            started.elapsed() < Duration::from_secs(2),
            "connect hung for {:?}",
            started.elapsed()
        );
    }

    #[tokio::test]
    async fn dropped_valkey_times_out_and_recovers() {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let live = Arc::new(AtomicBool::new(true));
        let accept_live = Arc::clone(&live);
        tokio::spawn(async move {
            loop {
                let Ok((sock, _)) = listener.accept().await else {
                    return;
                };
                let live = Arc::clone(&accept_live);
                tokio::spawn(handle_conn(sock, live));
            }
        });

        let limiter = RateLimiter::connect(&format!("redis://{addr}"))
            .await
            .expect("connect");
        limiter.ping().await.expect("ping");
        let verdict = limiter.check(&[bucket()]).await.expect("check");
        assert!(matches!(verdict, Verdict::Allowed(ref infos) if infos[0].remaining == 9));

        live.store(false, Ordering::SeqCst);
        let started = Instant::now();
        let ping_err = limiter.ping().await.expect_err("blackhole ping");
        assert!(
            ping_err.is_timeout() || ping_err.is_io_error(),
            "expected timeout, got {ping_err}"
        );
        assert!(
            started.elapsed() < Duration::from_secs(2),
            "ping hung for {:?}",
            started.elapsed()
        );
        let allow_started = Instant::now();
        let allow_err = limiter.check(&[bucket()]).await;
        assert_eq!(allow_err, Err(Unavailable));
        assert!(
            allow_started.elapsed() < Duration::from_secs(2),
            "allow hung for {:?}",
            allow_started.elapsed()
        );

        live.store(true, Ordering::SeqCst);
        let deadline = Instant::now() + Duration::from_secs(5);
        loop {
            if limiter.ping().await.is_ok() && limiter.check(&[bucket()]).await.is_ok() {
                break;
            }
            assert!(
                Instant::now() < deadline,
                "did not recover after valkey answered again"
            );
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
    }

    async fn handle_conn(mut sock: TcpStream, live: Arc<AtomicBool>) {
        let mut buf = Vec::new();
        let mut tmp = [0u8; 4096];
        loop {
            let n = match sock.read(&mut tmp).await {
                Ok(0) | Err(_) => return,
                Ok(n) => n,
            };
            if !live.load(Ordering::SeqCst) {
                loop {
                    if sock.read(&mut tmp).await.unwrap_or(0) == 0 {
                        return;
                    }
                }
            }
            buf.extend_from_slice(&tmp[..n]);
            let (names, consumed) = parse_commands(&buf);
            if consumed == 0 {
                continue;
            }
            buf.drain(..consumed);
            for name in names {
                let reply = match name.as_str() {
                    "PING" => "+PONG\r\n",
                    "EVAL" | "EVALSHA" => "*3\r\n:1\r\n:1\r\n:60\r\n",
                    _ => "+OK\r\n",
                };
                if sock.write_all(reply.as_bytes()).await.is_err() {
                    return;
                }
            }
        }
    }

    fn parse_commands(buf: &[u8]) -> (Vec<String>, usize) {
        let mut i = 0;
        let mut names = Vec::new();
        while i < buf.len() {
            if buf[i] != b'*' {
                break;
            }
            let Some(header_end) = find_crlf(buf, i) else {
                break;
            };
            let Some(n) = std::str::from_utf8(&buf[i + 1..header_end])
                .ok()
                .and_then(|s| s.parse::<usize>().ok())
            else {
                break;
            };
            let mut pos = header_end + 2;
            let mut args = Vec::new();
            let mut complete = true;
            for _ in 0..n {
                if pos >= buf.len() || buf[pos] != b'$' {
                    complete = false;
                    break;
                }
                let Some(len_end) = find_crlf(buf, pos) else {
                    complete = false;
                    break;
                };
                let Some(len) = std::str::from_utf8(&buf[pos + 1..len_end])
                    .ok()
                    .and_then(|s| s.parse::<usize>().ok())
                else {
                    complete = false;
                    break;
                };
                let data_start = len_end + 2;
                let data_end = data_start + len;
                if data_end + 2 > buf.len() {
                    complete = false;
                    break;
                }
                args.push(String::from_utf8_lossy(&buf[data_start..data_end]).into_owned());
                pos = data_end + 2;
            }
            if !complete {
                break;
            }
            names.push(args.first().cloned().unwrap_or_default());
            i = pos;
        }
        (names, i)
    }

    fn find_crlf(buf: &[u8], from: usize) -> Option<usize> {
        buf[from..]
            .windows(2)
            .position(|w| w == b"\r\n")
            .map(|p| from + p)
    }
}
