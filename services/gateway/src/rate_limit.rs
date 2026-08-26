use std::collections::HashMap;
use std::sync::Mutex;
use std::time::{Duration, Instant};

/// In-process fixed-window counter. No Redis — per gateway instance.
pub struct RateLimiter {
    inner: Mutex<HashMap<String, Window>>,
}

struct Window {
    start: Instant,
    count: u64,
}

impl RateLimiter {
    pub fn new() -> Self {
        Self {
            inner: Mutex::new(HashMap::new()),
        }
    }

    /// `limit == 0` is unlimited. On exceed, returns retry-after seconds (>= 1).
    pub fn allow(&self, key: &str, limit: u64, window_secs: u64) -> Result<(), u64> {
        if limit == 0 || window_secs == 0 {
            return Ok(());
        }
        let window = Duration::from_secs(window_secs);
        let now = Instant::now();
        let mut map = self.inner.lock().unwrap_or_else(|e| e.into_inner());
        if map.len() > 50_000 {
            map.retain(|_, w| now.duration_since(w.start) < window);
        }
        let entry = map.entry(key.to_string()).or_insert(Window {
            start: now,
            count: 0,
        });
        if now.duration_since(entry.start) >= window {
            entry.start = now;
            entry.count = 0;
        }
        if entry.count >= limit {
            let elapsed = now.duration_since(entry.start);
            let retry = window.saturating_sub(elapsed).as_secs().max(1);
            return Err(retry);
        }
        entry.count += 1;
        Ok(())
    }
}

impl Default for RateLimiter {
    fn default() -> Self {
        Self::new()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn unlimited_when_zero() {
        let lim = RateLimiter::new();
        for _ in 0..100 {
            lim.allow("k", 0, 60).unwrap();
        }
    }

    #[test]
    fn exceeds_returns_retry_after() {
        let lim = RateLimiter::new();
        lim.allow("k", 2, 60).unwrap();
        lim.allow("k", 2, 60).unwrap();
        let err = lim.allow("k", 2, 60).unwrap_err();
        assert!(err >= 1);
    }

    #[test]
    fn keys_are_independent() {
        let lim = RateLimiter::new();
        lim.allow("a", 1, 60).unwrap();
        lim.allow("b", 1, 60).unwrap();
        assert!(lim.allow("a", 1, 60).is_err());
        assert!(lim.allow("b", 1, 60).is_err());
    }
}
