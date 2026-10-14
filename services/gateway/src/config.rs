use std::env;
use std::time::Duration;

use crate::admission::parse_user_id_claim;

const DEFAULT_PORT: &str = "5001";
const DEFAULT_INTERNAL_PORT: &str = "8000";
const DEFAULT_USER_ID_CLAIM: &str = "properties.user_id";
const DEFAULT_APIKEY_CACHE_TTL_SECS: u64 = 300;
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
    /// The control plane's internal base URL (`CONTROL_PLANE_URL`): routes and
    /// CORS origins come from `{url}/internal/gateway`.
    pub control_plane_url: String,
    /// Valkey for rate-limit counters. Required.
    pub valkey_url: String,

    pub auth_issuer: String,
    pub auth_jwks_uri: String,
    pub auth_allowed_audiences: Vec<String>,
    /// Dotted claim path segments for Plat5 user id.
    pub auth_user_id_claim: Vec<String>,

    /// Identity's one credential validate (`CREDENTIAL_VALIDATE_URL`): every
    /// `X-API-Key`, and every JWT's issuer and subject. The gateway does not
    /// know the brand; identity does.
    pub credential_validate_url: String,
    pub internal_auth_token: Option<String>,

    /// TTL for every credential and IdP subject resolve (`APIKEY_CACHE_TTL_SECS`).
    pub apikey_cache_ttl_secs: u64,

    /// Defaults for a service whose route config sets no `timeouts`.
    pub upstream_connect_timeout: Duration,
    pub upstream_read_timeout: Duration,

    /// Gateway fallback. `0` = unlimited fallback (routes may still override).
    pub rate_limit_requests: u64,
    pub rate_limit_window_seconds: u64,
    pub rate_limit_auth_failure_requests: u64,
    pub rate_limit_auth_failure_window_seconds: u64,

    /// Audit's internal base URL. None = audit off (`AUDIT_ENABLED=false`).
    pub audit_url: Option<String>,
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
        let credential_validate_url = require_env("CREDENTIAL_VALIDATE_URL")?;
        let control_plane_url = require_env("CONTROL_PLANE_URL")?
            .trim()
            .trim_end_matches('/')
            .to_string();
        let valkey_url = require_env("VALKEY_URL")?;
        let audit_url = audit_url_from_env()?;
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
            control_plane_url,
            valkey_url,

            auth_issuer,
            auth_jwks_uri,
            auth_allowed_audiences: csv_env("AUTH_ALLOWED_AUDIENCES"),
            auth_user_id_claim: parse_user_id_claim(
                &env::var("AUTH_USER_ID_CLAIM")
                    .unwrap_or_else(|_| DEFAULT_USER_ID_CLAIM.to_string()),
            ),

            credential_validate_url,
            internal_auth_token: optional_nonempty_env("INTERNAL_AUTH_TOKEN"),

            apikey_cache_ttl_secs: parse_u64_env(
                "APIKEY_CACHE_TTL_SECS",
                DEFAULT_APIKEY_CACHE_TTL_SECS,
            )?,

            upstream_connect_timeout: Duration::from_millis(parse_u64_env(
                "UPSTREAM_CONNECT_TIMEOUT_MS",
                DEFAULT_UPSTREAM_CONNECT_TIMEOUT_MS,
            )?),
            upstream_read_timeout: Duration::from_millis(parse_u64_env(
                "UPSTREAM_READ_TIMEOUT_MS",
                DEFAULT_UPSTREAM_READ_TIMEOUT_MS,
            )?),

            rate_limit_requests,
            rate_limit_window_seconds,
            rate_limit_auth_failure_requests,
            rate_limit_auth_failure_window_seconds,

            audit_url,
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

/// Audit is on unless `AUDIT_ENABLED=false`. On, `AUDIT_URL` is required: a
/// forgotten URL fails boot instead of silently turning audit off.
fn audit_url_from_env() -> Result<Option<String>, GatewayConfigError> {
    parse_audit(
        env::var("AUDIT_ENABLED").ok().as_deref(),
        env::var("AUDIT_URL").ok().as_deref(),
    )
}

fn parse_audit(
    enabled: Option<&str>,
    url: Option<&str>,
) -> Result<Option<String>, GatewayConfigError> {
    match enabled.map(str::trim) {
        None | Some("") | Some("true") => {}
        Some("false") => return Ok(None),
        Some(other) => {
            return Err(GatewayConfigError::Invalid {
                key: "AUDIT_ENABLED",
                message: format!("must be true or false, got {other:?}"),
            })
        }
    }
    match url.map(str::trim) {
        Some(u) if !u.is_empty() => Ok(Some(u.trim_end_matches('/').to_string())),
        _ => Err(GatewayConfigError::Missing("AUDIT_URL")),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn audit_is_on_by_default_and_needs_a_url() {
        assert_eq!(
            parse_audit(None, Some("http://audit:3003/"))
                .unwrap()
                .as_deref(),
            Some("http://audit:3003")
        );
        assert_eq!(
            parse_audit(Some("true"), Some("http://audit:3003"))
                .unwrap()
                .as_deref(),
            Some("http://audit:3003")
        );
        for url in [None, Some(""), Some("  ")] {
            assert!(matches!(
                parse_audit(None, url),
                Err(GatewayConfigError::Missing("AUDIT_URL"))
            ));
        }
    }

    #[test]
    fn audit_off_ignores_the_url() {
        assert_eq!(parse_audit(Some("false"), None).unwrap(), None);
        assert_eq!(
            parse_audit(Some("false"), Some("http://audit:3003")).unwrap(),
            None
        );
        assert!(parse_audit(Some("no"), Some("http://audit:3003")).is_err());
    }
}
