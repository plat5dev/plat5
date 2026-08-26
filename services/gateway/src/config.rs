use std::env;
use std::time::Duration;

use crate::admission::parse_user_id_claim;

const DEFAULT_PORT: &str = "5001";
const DEFAULT_INTERNAL_PORT: &str = "8000";
const DEFAULT_ETCD_URL: &str = "http://localhost:2379";
const DEFAULT_USER_ID_CLAIM: &str = "properties.user_id";
const DEFAULT_APIKEY_BRAND: &str = "plat5";
const MAX_APIKEY_BRAND_LEN: usize = 32;
const DEFAULT_APIKEY_CACHE_TTL_SECS: u64 = 300;
const DEFAULT_MEMBER_CACHE_TTL_SECS: u64 = 300;
const DEFAULT_UPSTREAM_CONNECT_TIMEOUT_MS: u64 = 10_000;
const DEFAULT_UPSTREAM_READ_TIMEOUT_MS: u64 = 30_000;
const DEFAULT_RATE_LIMIT_REQUESTS: u64 = 60;
const DEFAULT_RATE_LIMIT_WINDOW_SECONDS: u64 = 60;
const DEFAULT_RATE_LIMIT_AUTH_FAILURE_REQUESTS: u64 = 60;
const DEFAULT_RATE_LIMIT_AUTH_FAILURE_WINDOW_SECONDS: u64 = 60;

/// Process configuration loaded from environment.
#[derive(Clone, Debug)]
pub struct GatewayConfig {
    pub port: String,
    pub internal_port: String,
    pub etcd_url: String,

    pub auth_issuer: String,
    pub auth_jwks_uri: String,
    pub auth_allowed_audiences: Vec<String>,
    /// Dotted claim path segments for Plat5 user id.
    pub auth_user_id_claim: Vec<String>,

    pub user_apikey_validate_url: String,
    pub member_apikey_validate_url: Option<String>,
    pub member_resolve_url: Option<String>,
    pub internal_auth_token: Option<String>,

    /// `APIKEY_BRAND`; same value as identity. Unset → `plat5`.
    pub apikey_brand: String,
    /// `{brand}-sk-1-`
    pub user_key_prefix: String,
    /// `{brand}-mk-1-`
    pub member_key_prefix: String,

    /// TTL for user + member API key caches (`APIKEY_CACHE_TTL_SECS`).
    pub apikey_cache_ttl_secs: u64,
    pub member_cache_ttl_secs: u64,

    pub upstream_connect_timeout: Duration,
    pub upstream_read_timeout: Duration,
    /// Empty = CORS `*`. Non-empty = allowlist.
    pub allowed_origins: Vec<String>,

    /// Gateway fallback. `0` = unlimited fallback (routes may still override).
    /// Subject follows route scope (public→ip, user→user, organization→org).
    pub rate_limit_requests: u64,
    pub rate_limit_window_seconds: u64,
    pub rate_limit_auth_failure_requests: u64,
    pub rate_limit_auth_failure_window_seconds: u64,
}

#[derive(Debug)]
pub enum GatewayConfigError {
    Missing(&'static str),
    Invalid { key: &'static str, message: String },
}

impl std::fmt::Display for GatewayConfigError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            GatewayConfigError::Missing(key) => write!(f, "{key} is not set"),
            GatewayConfigError::Invalid { key, message } => write!(f, "invalid {key}: {message}"),
        }
    }
}

impl std::error::Error for GatewayConfigError {}

impl GatewayConfig {
    pub fn from_env() -> Result<Self, GatewayConfigError> {
        let auth_issuer = require_env("AUTH_ISSUER")?;
        let auth_jwks_uri = require_env("AUTH_JWKS_URI")?;
        let user_apikey_validate_url = require_env("USER_APIKEY_VALIDATE_URL")?;
        let apikey_brand = apikey_brand_from_env()?;
        let rate_limit_requests =
            parse_u64_env("RATE_LIMIT_REQUESTS", DEFAULT_RATE_LIMIT_REQUESTS)?;
        let rate_limit_window_seconds = parse_u64_env(
            "RATE_LIMIT_WINDOW_SECONDS",
            DEFAULT_RATE_LIMIT_WINDOW_SECONDS,
        )?;
        if rate_limit_requests > 0 && rate_limit_window_seconds == 0 {
            return Err(GatewayConfigError::Invalid {
                key: "RATE_LIMIT_WINDOW_SECONDS",
                message: "must be > 0 when RATE_LIMIT_REQUESTS > 0".to_string(),
            });
        }
        let rate_limit_auth_failure_requests = parse_u64_env(
            "RATE_LIMIT_AUTH_FAILURE_REQUESTS",
            DEFAULT_RATE_LIMIT_AUTH_FAILURE_REQUESTS,
        )?;
        let rate_limit_auth_failure_window_seconds = parse_u64_env(
            "RATE_LIMIT_AUTH_FAILURE_WINDOW_SECONDS",
            DEFAULT_RATE_LIMIT_AUTH_FAILURE_WINDOW_SECONDS,
        )?;
        if rate_limit_auth_failure_requests > 0 && rate_limit_auth_failure_window_seconds == 0 {
            return Err(GatewayConfigError::Invalid {
                key: "RATE_LIMIT_AUTH_FAILURE_WINDOW_SECONDS",
                message: "must be > 0 when RATE_LIMIT_AUTH_FAILURE_REQUESTS > 0".to_string(),
            });
        }

        Ok(Self {
            port: env::var("PORT").unwrap_or_else(|_| DEFAULT_PORT.to_string()),
            internal_port: env::var("INTERNAL_PORT")
                .unwrap_or_else(|_| DEFAULT_INTERNAL_PORT.to_string()),
            etcd_url: env::var("ETCD_URL").unwrap_or_else(|_| DEFAULT_ETCD_URL.to_string()),

            auth_issuer,
            auth_jwks_uri,
            auth_allowed_audiences: csv_env("AUTH_ALLOWED_AUDIENCES"),
            auth_user_id_claim: parse_user_id_claim(
                &env::var("AUTH_USER_ID_CLAIM")
                    .unwrap_or_else(|_| DEFAULT_USER_ID_CLAIM.to_string()),
            ),

            user_apikey_validate_url,
            member_apikey_validate_url: optional_nonempty_env("MEMBER_APIKEY_VALIDATE_URL"),
            member_resolve_url: optional_nonempty_env("MEMBER_RESOLVE_URL"),
            internal_auth_token: optional_nonempty_env("INTERNAL_AUTH_TOKEN"),

            apikey_brand: apikey_brand.clone(),
            user_key_prefix: user_apikey_prefix(&apikey_brand),
            member_key_prefix: member_apikey_prefix(&apikey_brand),

            apikey_cache_ttl_secs: parse_u64_env(
                "APIKEY_CACHE_TTL_SECS",
                DEFAULT_APIKEY_CACHE_TTL_SECS,
            )?,
            member_cache_ttl_secs: parse_u64_env(
                "MEMBER_CACHE_TTL_SECS",
                DEFAULT_MEMBER_CACHE_TTL_SECS,
            )?,

            upstream_connect_timeout: Duration::from_millis(parse_u64_env(
                "UPSTREAM_CONNECT_TIMEOUT_MS",
                DEFAULT_UPSTREAM_CONNECT_TIMEOUT_MS,
            )?),
            upstream_read_timeout: Duration::from_millis(parse_u64_env(
                "UPSTREAM_READ_TIMEOUT_MS",
                DEFAULT_UPSTREAM_READ_TIMEOUT_MS,
            )?),
            allowed_origins: csv_env("ALLOWED_ORIGINS"),

            rate_limit_requests,
            rate_limit_window_seconds,
            rate_limit_auth_failure_requests,
            rate_limit_auth_failure_window_seconds,
        })
    }
}

fn require_env(key: &'static str) -> Result<String, GatewayConfigError> {
    env::var(key).map_err(|_| GatewayConfigError::Missing(key))
}

fn optional_nonempty_env(key: &str) -> Option<String> {
    env::var(key).ok().filter(|s| !s.is_empty())
}

fn csv_env(key: &str) -> Vec<String> {
    env::var(key)
        .unwrap_or_default()
        .split(',')
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
        .collect()
}

fn parse_u64_env(key: &'static str, default: u64) -> Result<u64, GatewayConfigError> {
    match env::var(key) {
        Ok(raw) => raw.trim().parse().map_err(|e| GatewayConfigError::Invalid {
            key,
            message: format!("{e}"),
        }),
        Err(_) => Ok(default),
    }
}

fn apikey_brand_from_env() -> Result<String, GatewayConfigError> {
    match env::var("APIKEY_BRAND") {
        Ok(raw) => parse_apikey_brand(&raw),
        Err(_) => Ok(DEFAULT_APIKEY_BRAND.to_string()),
    }
}

/// `[a-z][a-z0-9]*`, max 32. No case folding. Same rule as identity.
fn parse_apikey_brand(raw: &str) -> Result<String, GatewayConfigError> {
    let s = raw.trim();
    if s.is_empty() {
        return Err(GatewayConfigError::Invalid {
            key: "APIKEY_BRAND",
            message: "is empty".to_string(),
        });
    }
    if s.len() > MAX_APIKEY_BRAND_LEN {
        return Err(GatewayConfigError::Invalid {
            key: "APIKEY_BRAND",
            message: format!("longer than {MAX_APIKEY_BRAND_LEN} characters"),
        });
    }
    let mut chars = s.chars();
    let first = chars.next().expect("non-empty");
    if !first.is_ascii_lowercase() || !chars.all(|c| c.is_ascii_lowercase() || c.is_ascii_digit()) {
        return Err(GatewayConfigError::Invalid {
            key: "APIKEY_BRAND",
            message: format!("must be [a-z][a-z0-9]*, max {MAX_APIKEY_BRAND_LEN}"),
        });
    }
    Ok(s.to_string())
}

fn user_apikey_prefix(brand: &str) -> String {
    format!("{brand}-sk-1-")
}

fn member_apikey_prefix(brand: &str) -> String {
    format!("{brand}-mk-1-")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_apikey_brand_ok() {
        for brand in ["plat5", "acme", "a", "a1", "happ", "sk"] {
            assert_eq!(parse_apikey_brand(brand).unwrap(), brand);
        }
        assert_eq!(parse_apikey_brand("  acme  ").unwrap(), "acme");
    }

    #[test]
    fn parse_apikey_brand_rejects() {
        for brand in ["", "   ", "Plat5", "acme-app", "1acme", "-x"] {
            assert!(
                parse_apikey_brand(brand).is_err(),
                "expected error for {brand:?}"
            );
        }
        let too_long = "a".repeat(MAX_APIKEY_BRAND_LEN + 1);
        assert!(parse_apikey_brand(&too_long).is_err());
        let max = "a".repeat(MAX_APIKEY_BRAND_LEN);
        assert_eq!(parse_apikey_brand(&max).unwrap(), max);
    }

    #[test]
    fn wire_prefixes() {
        assert_eq!(user_apikey_prefix("plat5"), "plat5-sk-1-");
        assert_eq!(member_apikey_prefix("acme"), "acme-mk-1-");
    }
}
