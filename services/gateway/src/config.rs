use std::env;
use std::time::Duration;

use crate::admission::parse_user_id_claim;

const DEFAULT_PORT: &str = "5001";
const DEFAULT_INTERNAL_PORT: &str = "8000";
const DEFAULT_ETCD_URL: &str = "http://localhost:2379";
const DEFAULT_USER_ID_CLAIM: &str = "properties.user_id";
const DEFAULT_APIKEY_CACHE_TTL_SECS: u64 = 300;
const DEFAULT_MEMBER_CACHE_TTL_SECS: u64 = 300;
const DEFAULT_UPSTREAM_CONNECT_TIMEOUT_MS: u64 = 10_000;
const DEFAULT_UPSTREAM_READ_TIMEOUT_MS: u64 = 30_000;

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

    /// TTL for user + member API key caches (`APIKEY_CACHE_TTL_SECS`).
    pub apikey_cache_ttl_secs: u64,
    pub member_cache_ttl_secs: u64,

    pub upstream_connect_timeout: Duration,
    pub upstream_read_timeout: Duration,
    /// Empty = CORS `*`. Non-empty = allowlist.
    pub allowed_origins: Vec<String>,
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
