use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use arc_swap::ArcSwap;
use redis::aio::{ConnectionManager, ConnectionManagerConfig};
use redis::{Client, RedisError, Script};
use std::sync::LazyLock;
use tokio::sync::Notify;
use tracing::{info, warn};

use crate::metrics;

/// Default counter key prefix (`VALKEY_KEY_PREFIX`), so a Valkey shared with other software is safe.
pub const DEFAULT_KEY_PREFIX: &str = "plat5:rl:";

/// Command and connect bound. A dead socket must surface as an error inside this window.
const VALKEY_TIMEOUT: Duration = Duration::from_millis(500);

/// Backoff sequence for the background reconnect loop after Valkey fails.
const RECONNECT_BACKOFF: &[Duration] = &[
    Duration::from_millis(200),
    Duration::from_millis(500),
    Duration::from_secs(1),
    Duration::from_secs(2),
    Duration::from_secs(5),
];

/// Cluster-wide fixed-window limiter backed by Valkey, or disabled when
/// `RATE_LIMIT_ENABLED=false`. A request's buckets are checked together in one
/// round trip and counted only if every one passes.
///
/// Clones share one connection. When Valkey becomes unreachable, the limiter
/// marks itself down so subsequent checks return [`Unavailable`] immediately
/// (0ms) for the pipeline to fail open, while a single background loop
/// reconnects with capped backoff and restores enforcement once Valkey answers.
#[derive(Clone)]
pub struct RateLimiter {
    inner: Option<Arc<Inner>>,
}

struct Inner {
    client: Client,
    conn: ArcSwap<Option<ConnectionManager>>,
    up: AtomicBool,
    reconnect: Arc<Notify>,
    key_prefix: String,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct RateLimitInfo {
    pub limit: u64,
    pub remaining: u64,
    pub reset_epoch: u64,
}

/// One fixed-window counter: `key` under the configured Valkey key prefix.
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

/// Valkey didn't answer in time, or is currently disconnected: caller fails open.
#[derive(Debug, PartialEq, Eq)]
pub struct Unavailable;

impl RateLimiter {
    /// Connect eagerly, failing if `url` is invalid or Valkey does not answer.
    pub async fn connect(url: &str, key_prefix: String) -> Result<Self, RedisError> {
        let client = Client::open(url)?;
        let conn = open_manager(client.clone()).await?;
        metrics::record_valkey_up(true);
        let inner = Arc::new(Inner {
            client,
            conn: ArcSwap::from_pointee(Some(conn)),
            up: AtomicBool::new(true),
            reconnect: Arc::new(Notify::new()),
            key_prefix,
        });
        spawn_reconnect_loop(&inner, &tokio::runtime::Handle::current());
        Ok(Self { inner: Some(inner) })
    }

    /// Validate `url` eagerly and attempt an initial connect. If Valkey is
    /// unreachable at boot, starts degraded (failing open) and reconnects in
    /// the background on `runtime`.
    pub async fn connect_or_start(
        url: &str,
        key_prefix: String,
        runtime: &tokio::runtime::Handle,
    ) -> Result<Self, RedisError> {
        let client = Client::open(url)?;
        let (conn, up) = match open_manager(client.clone()).await {
            Ok(conn) => {
                metrics::record_valkey_up(true);
                (Some(conn), true)
            }
            Err(err) => {
                warn!(
                    error = %err,
                    "valkey unavailable at startup; rate limiting failing open until reconnected"
                );
                metrics::record_valkey_up(false);
                (None, false)
            }
        };
        let inner = Arc::new(Inner {
            client,
            conn: ArcSwap::from_pointee(conn),
            up: AtomicBool::new(up),
            reconnect: Arc::new(Notify::new()),
            key_prefix,
        });
        spawn_reconnect_loop(&inner, runtime);
        if !up {
            inner.reconnect.notify_one();
        }
        Ok(Self { inner: Some(inner) })
    }

    pub fn disabled() -> Self {
        Self { inner: None }
    }

    pub fn is_enabled(&self) -> bool {
        self.inner.is_some()
    }

    /// Whether Valkey is currently reachable. When already marked down, returns
    /// an error immediately instead of waiting on a dead socket.
    pub async fn ping(&self) -> Result<(), RedisError> {
        let Some(inner) = self.inner.as_ref() else {
            return Ok(());
        };
        if !inner.up.load(Ordering::Acquire) {
            return Err(RedisError::from(std::io::Error::new(
                std::io::ErrorKind::NotConnected,
                "valkey disconnected",
            )));
        }
        let guard = inner.conn.load();
        let Some(existing) = guard.as_ref().as_ref() else {
            return Err(RedisError::from(std::io::Error::new(
                std::io::ErrorKind::NotConnected,
                "valkey disconnected",
            )));
        };
        let mut conn = existing.clone();
        let result = redis::cmd("PING").query_async(&mut conn).await;
        if let Err(ref err) = result {
            note_disconnect(inner, err);
        }
        result
    }

    /// Check every bucket, and count them all only if all pass. No buckets or
    /// disabled: no trip. When Valkey is marked down, returns [`Unavailable`]
    /// immediately (0ms) so the caller can fail open without paying a timeout.
    pub async fn check(&self, buckets: &[Bucket]) -> Result<Verdict, Unavailable> {
        let Some(inner) = self.inner.as_ref() else {
            return Ok(Verdict::Allowed(Vec::new()));
        };
        if buckets.is_empty() {
            return Ok(Verdict::Allowed(Vec::new()));
        }
        if !inner.up.load(Ordering::Acquire) {
            return Err(Unavailable);
        }
        let guard = inner.conn.load();
        let Some(existing) = guard.as_ref().as_ref() else {
            return Err(Unavailable);
        };
        let mut invocation = CHECK.prepare_invoke();
        for bucket in buckets {
            invocation
                .key(format!("{}{}", inner.key_prefix, bucket.key))
                .arg(bucket.requests)
                .arg(bucket.window_seconds);
        }
        let mut conn = existing.clone();
        match invocation.invoke_async::<Vec<i64>>(&mut conn).await {
            Ok(vals) if vals.len() == 1 + 2 * buckets.len() => {
                Ok(verdict(buckets, vals[0] == 1, &vals[1..], now_epoch()))
            }
            Ok(_) => {
                warn!("rate limit script returned unexpected value");
                Err(Unavailable)
            }
            Err(err) => {
                note_disconnect(inner, &err);
                Err(Unavailable)
            }
        }
    }
}

/// A response timeout or broken socket flips the breaker to down (logging once
/// on the transition) and wakes the single background reconnect loop.
fn note_disconnect(inner: &Arc<Inner>, err: &RedisError) {
    if err.is_io_error() || err.is_unrecoverable_error() || err.is_timeout() {
        if inner.up.swap(false, Ordering::AcqRel) {
            warn!(error = %err, "valkey unavailable; rate limiting failing open");
            metrics::record_valkey_up(false);
        }
        inner.reconnect.notify_one();
    } else {
        warn!(error = %err, "rate limit valkey error");
    }
}

fn spawn_reconnect_loop(inner: &Arc<Inner>, runtime: &tokio::runtime::Handle) {
    let weak = Arc::downgrade(inner);
    let reconnect = Arc::clone(&inner.reconnect);
    runtime.spawn(async move {
        loop {
            let is_up = match weak.upgrade() {
                Some(inner) => inner.up.load(Ordering::Acquire),
                None => return,
            };
            if is_up {
                reconnect.notified().await;
                continue;
            }
            let mut attempt = 0usize;
            loop {
                let delay = RECONNECT_BACKOFF[attempt.min(RECONNECT_BACKOFF.len() - 1)];
                attempt = attempt.saturating_add(1);
                tokio::time::sleep(delay).await;
                let Some(inner) = weak.upgrade() else {
                    return;
                };
                if inner.up.load(Ordering::Acquire) {
                    break;
                }
                match open_manager(inner.client.clone()).await {
                    Ok(mut conn) => {
                        if redis::cmd("PING")
                            .query_async::<String>(&mut conn)
                            .await
                            .is_ok()
                        {
                            inner.conn.store(Arc::new(Some(conn)));
                            inner.up.store(true, Ordering::Release);
                            metrics::record_valkey_up(true);
                            info!("valkey reconnected; rate limiting restored");
                            break;
                        }
                    }
                    Err(err) if attempt == 1 => {
                        warn!(error = %err, "valkey reconnect attempt failed; retrying in background");
                    }
                    Err(_) => {}
                }
            }
        }
    });
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

static CHECK: LazyLock<Script> = LazyLock::new(|| Script::new(CHECK_SCRIPT));

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
        let result =
            RateLimiter::connect(&format!("redis://{addr}"), DEFAULT_KEY_PREFIX.to_string()).await;
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

        let limiter =
            RateLimiter::connect(&format!("redis://{addr}"), DEFAULT_KEY_PREFIX.to_string())
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

    #[tokio::test]
    async fn connect_or_start_degrades_and_recovers_with_zero_ms_checks() {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let live = Arc::new(AtomicBool::new(false));
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

        let limiter = RateLimiter::connect_or_start(
            &format!("redis://{addr}"),
            DEFAULT_KEY_PREFIX.to_string(),
            &tokio::runtime::Handle::current(),
        )
        .await
        .expect("connect_or_start succeeds even when valkey is down");

        // While down, check() fails open immediately without waiting 500ms.
        let started = Instant::now();
        assert_eq!(limiter.check(&[bucket()]).await, Err(Unavailable));
        assert!(
            started.elapsed() < Duration::from_millis(50),
            "check blocked while down: {:?}",
            started.elapsed()
        );
        assert!(limiter.ping().await.is_err());

        live.store(true, Ordering::SeqCst);
        let deadline = Instant::now() + Duration::from_secs(5);
        loop {
            if limiter.ping().await.is_ok() && limiter.check(&[bucket()]).await.is_ok() {
                break;
            }
            assert!(Instant::now() < deadline, "did not reconnect");
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
