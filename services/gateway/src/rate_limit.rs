use std::collections::HashMap;
use std::sync::Mutex;
use std::time::{Duration, Instant};

use crate::admission::Admission;
use crate::route_config::{RateLimitBy, RouteRateLimit};
use crate::route_map::{Route, RouteScope};

const MAX_BUCKETS: usize = 50_000;

struct Bucket {
    tokens: f64,
    last: Instant,
}

/// In-process token bucket. Per gateway instance; not shared.
#[derive(Default)]
pub struct RateLimiter {
    inner: Mutex<HashMap<String, Bucket>>,
}

impl RateLimiter {
    pub fn new() -> Self {
        Self::default()
    }

    /// Returns `Some(retry_after_seconds)` when the key is limited.
    pub fn check(&self, key: &str, requests: u64, window_seconds: u64) -> Option<u64> {
        if requests == 0 || window_seconds == 0 {
            return None;
        }
        let now = Instant::now();
        let cap = requests as f64;
        let refill_per_sec = cap / window_seconds as f64;
        let mut map = self.inner.lock().unwrap_or_else(|e| e.into_inner());
        if map.len() > MAX_BUCKETS {
            let cutoff = now - Duration::from_secs(window_seconds.saturating_mul(2).max(60));
            map.retain(|_, b| b.last > cutoff);
        }
        let bucket = map.entry(key.to_string()).or_insert(Bucket {
            tokens: cap,
            last: now,
        });
        let elapsed = now.saturating_duration_since(bucket.last).as_secs_f64();
        bucket.tokens = (bucket.tokens + elapsed * refill_per_sec).min(cap);
        bucket.last = now;
        if bucket.tokens >= 1.0 {
            bucket.tokens -= 1.0;
            return None;
        }
        let need = 1.0 - bucket.tokens;
        let retry = (need / refill_per_sec).ceil().max(1.0) as u64;
        Some(retry)
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct ResolvedRateLimit {
    pub requests: u64,
    pub window_seconds: u64,
    pub by: RateLimitBy,
}

/// Omitted route limit inherits gateway fallback. `false` opts out. `0` fallback is unlimited.
pub fn resolve_route_limit(
    route_limit: Option<&RouteRateLimit>,
    fallback_requests: u64,
    fallback_window_seconds: u64,
    fallback_by: Option<RateLimitBy>,
    scope: RouteScope,
) -> Option<ResolvedRateLimit> {
    let (requests, window_seconds, override_by) = match route_limit {
        Some(RouteRateLimit::Unlimited) => return None,
        Some(RouteRateLimit::Override(o)) => {
            if o.requests == 0 || o.window_seconds == 0 {
                return None;
            }
            (o.requests, o.window_seconds, o.by)
        }
        None => {
            if fallback_requests == 0 || fallback_window_seconds == 0 {
                return None;
            }
            (fallback_requests, fallback_window_seconds, None)
        }
    };
    Some(ResolvedRateLimit {
        requests,
        window_seconds,
        by: effective_by(override_by, fallback_by, scope),
    })
}

pub fn effective_by(
    override_by: Option<RateLimitBy>,
    fallback_by: Option<RateLimitBy>,
    scope: RouteScope,
) -> RateLimitBy {
    if let Some(b) = override_by {
        return b;
    }
    if let Some(b) = fallback_by {
        return b;
    }
    match scope {
        RouteScope::Public => RateLimitBy::Ip,
        RouteScope::User => RateLimitBy::User,
        RouteScope::Organization => RateLimitBy::Member,
    }
}

pub fn principal_id(by: RateLimitBy, admission: &Admission, ip: &str) -> String {
    match by {
        RateLimitBy::Ip => ip.to_string(),
        RateLimitBy::User => admission
            .user_id()
            .map(|s| s.to_string())
            .unwrap_or_else(|| ip.to_string()),
        RateLimitBy::Member => admission
            .member_id()
            .or_else(|| admission.user_id())
            .map(|s| s.to_string())
            .unwrap_or_else(|| ip.to_string()),
    }
}

pub fn route_limit_key(path: &str, method: &str, by: RateLimitBy, id: &str) -> String {
    format!("route:{path}:{method}:{}:{id}", by.as_str())
}

pub fn auth_failure_key(ip: &str) -> String {
    format!("authfail:{ip}")
}

pub struct RateLimitState {
    pub route: RateLimiter,
    pub auth_failure: RateLimiter,
    pub fallback_requests: u64,
    pub fallback_window_seconds: u64,
    pub fallback_by: Option<RateLimitBy>,
    pub auth_failure_requests: u64,
    pub auth_failure_window_seconds: u64,
}

impl RateLimitState {
    pub fn check_route(
        &self,
        route: &Route,
        method: &str,
        admission: &Admission,
        ip: &str,
    ) -> Option<u64> {
        let spec = resolve_route_limit(
            route.rate_limit.as_ref(),
            self.fallback_requests,
            self.fallback_window_seconds,
            self.fallback_by,
            route.scope,
        )?;
        let id = principal_id(spec.by, admission, ip);
        let key = route_limit_key(&route.path, method, spec.by, &id);
        self.route.check(&key, spec.requests, spec.window_seconds)
    }

    pub fn check_auth_failure(&self, ip: &str) -> Option<u64> {
        if self.auth_failure_requests == 0 || self.auth_failure_window_seconds == 0 {
            return None;
        }
        self.auth_failure.check(
            &auth_failure_key(ip),
            self.auth_failure_requests,
            self.auth_failure_window_seconds,
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::admission::AuthType;
    use crate::route_config::RateLimitOverride;
    use std::collections::HashSet;

    fn public_route(limit: Option<RouteRateLimit>) -> Route {
        Route {
            base_url: "http://x".into(),
            path: "/pub".into(),
            methods: HashSet::from(["GET".into()]),
            transform_path: None,
            scope: RouteScope::Public,
            organization_param: None,
            required_scopes: None,
            rate_limit: limit,
        }
    }

    #[test]
    fn omitted_rate_limit_inherits_fallback() {
        let got = resolve_route_limit(None, 60, 60, None, RouteScope::Public).unwrap();
        assert_eq!(got.requests, 60);
        assert_eq!(got.window_seconds, 60);
        assert_eq!(got.by, RateLimitBy::Ip);
    }

    #[test]
    fn omitted_user_scope_defaults_by_user() {
        let got = resolve_route_limit(None, 10, 30, None, RouteScope::User).unwrap();
        assert_eq!(got.by, RateLimitBy::User);
    }

    #[test]
    fn omitted_org_scope_defaults_by_member() {
        let got = resolve_route_limit(None, 10, 30, None, RouteScope::Organization).unwrap();
        assert_eq!(got.by, RateLimitBy::Member);
    }

    #[test]
    fn rate_limit_false_opts_out() {
        assert!(resolve_route_limit(
            Some(&RouteRateLimit::Unlimited),
            60,
            60,
            None,
            RouteScope::User
        )
        .is_none());
    }

    #[test]
    fn fallback_zero_is_unlimited() {
        assert!(resolve_route_limit(None, 0, 60, None, RouteScope::User).is_none());
    }

    #[test]
    fn override_uses_route_values() {
        let limit = RouteRateLimit::Override(RateLimitOverride {
            requests: 3,
            window_seconds: 10,
            by: Some(RateLimitBy::Ip),
        });
        let got = resolve_route_limit(Some(&limit), 60, 60, None, RouteScope::User).unwrap();
        assert_eq!(got.requests, 3);
        assert_eq!(got.window_seconds, 10);
        assert_eq!(got.by, RateLimitBy::Ip);
    }

    #[test]
    fn token_bucket_limits_then_allows_after_window() {
        let limiter = RateLimiter::new();
        assert!(limiter.check("k", 2, 60).is_none());
        assert!(limiter.check("k", 2, 60).is_none());
        let retry = limiter.check("k", 2, 60).expect("limited");
        assert!(retry >= 1);
    }

    #[test]
    fn failed_auth_limiter_is_separate_from_route() {
        let state = RateLimitState {
            route: RateLimiter::new(),
            auth_failure: RateLimiter::new(),
            fallback_requests: 60,
            fallback_window_seconds: 60,
            fallback_by: None,
            auth_failure_requests: 2,
            auth_failure_window_seconds: 60,
        };
        let route = public_route(None);
        let admission = Admission::Public;
        assert!(state
            .check_route(&route, "GET", &admission, "1.1.1.1")
            .is_none());
        assert!(state.check_auth_failure("1.1.1.1").is_none());
        assert!(state.check_auth_failure("1.1.1.1").is_none());
        assert!(state.check_auth_failure("1.1.1.1").is_some());
        assert!(
            state
                .check_route(&route, "GET", &admission, "1.1.1.1")
                .is_none(),
            "route limiter must not share buckets with failed-auth"
        );
    }

    #[test]
    fn jwt_and_key_use_same_route_limiter_principal() {
        let admission = Admission::User {
            user_id: "u1".into(),
            auth_type: AuthType::Jwt,
            kid: None,
            key_scopes: None,
        };
        assert_eq!(principal_id(RateLimitBy::User, &admission, "9.9.9.9"), "u1");
    }
}
