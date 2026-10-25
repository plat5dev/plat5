use std::env;
use std::time::Duration;

use crate::auth::jwt::Audiences;
use crate::auth::parse_subject_claim;
use crate::gateway::TrustedProxies;

const DEFAULT_PORT: &str = "5001";
const DEFAULT_INTERNAL_PORT: &str = "8000";
const DEFAULT_SUBJECT_CLAIM: &str = "sub";
const DEFAULT_ADMIT_CACHE_TTL: Duration = Duration::from_secs(5 * 60);
const DEFAULT_ADMIT_NEGATIVE_CACHE_TTL: Duration = Duration::from_secs(10);
const DEFAULT_RATE_LIMIT_AUTH_FAILURE_REQUESTS: u64 = 60;
const DEFAULT_RATE_LIMIT_AUTH_FAILURE_WINDOW_SECONDS: u64 = 60;

/// Process configuration loaded from environment (docs/reference/env.md#gateway).
#[derive(Clone, Debug)]
pub struct GatewayConfig {
    pub port: String,
    pub internal_port: String,
    /// The control plane's internal base URL (`CONTROL_PLANE_URL`): kinds, routes,
    /// rate limits and CORS come from `{url}/internal/gateway`.
    pub control_plane_url: String,
    /// Valkey for rate-limit counters. Required.
    pub valkey_url: String,

    pub auth_issuer: String,
    pub auth_jwks_uri: String,
    pub auth_allowed_audiences: Audiences,
    /// Dotted claim path segments for the person's IdP subject (`AUTH_SUBJECT_CLAIM`).
    pub auth_subject_claim: Vec<String>,

    /// Identity's internal base URL (`IDENTITY_INTERNAL_URL`); admit is at `/internal/admit`.
    pub identity_internal_url: String,

    /// How long an admit answer is kept (`ADMIT_CACHE_TTL`).
    pub admit_cache_ttl: Duration,
    /// How long a refusal is kept (`ADMIT_NEGATIVE_CACHE_TTL`).
    pub admit_negative_cache_ttl: Duration,

    pub rate_limit_auth_failure_requests: u64,
    pub rate_limit_auth_failure_window_seconds: u64,

    /// Audit's internal base URL. None = audit off (`AUDIT_ENABLED=false`).
    pub audit_url: Option<String>,

    /// Proxies whose `X-Forwarded-For` names the client (`TRUSTED_PROXIES`).
    /// Empty = the TCP peer is the client.
    pub trusted_proxies: TrustedProxies,
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
        let auth_allowed_audiences = parse_audiences(&require_env("AUTH_ALLOWED_AUDIENCES")?)
            .ok_or(GatewayConfigError::Invalid {
                key: "AUTH_ALLOWED_AUDIENCES",
                message: "list at least one audience, or *".to_string(),
            })?;
        let identity_internal_url = base_url(require_env("IDENTITY_INTERNAL_URL")?);
        let control_plane_url = base_url(require_env("CONTROL_PLANE_URL")?);
        let valkey_url = require_env("VALKEY_URL")?;
        let audit_url = audit_url_from_env()?;
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

        let trusted_proxies = TrustedProxies::parse(
            &env::var("TRUSTED_PROXIES").unwrap_or_default(),
        )
        .map_err(|message| GatewayConfigError::Invalid {
            key: "TRUSTED_PROXIES",
            message,
        })?;

        Ok(Self {
            port: env::var("PORT").unwrap_or_else(|_| DEFAULT_PORT.to_string()),
            internal_port: env::var("INTERNAL_PORT")
                .unwrap_or_else(|_| DEFAULT_INTERNAL_PORT.to_string()),
            control_plane_url,
            valkey_url,

            auth_issuer,
            auth_jwks_uri,
            auth_allowed_audiences,
            auth_subject_claim: parse_subject_claim(
                &env::var("AUTH_SUBJECT_CLAIM")
                    .unwrap_or_else(|_| DEFAULT_SUBJECT_CLAIM.to_string()),
            ),

            identity_internal_url,

            admit_cache_ttl: duration_env("ADMIT_CACHE_TTL", DEFAULT_ADMIT_CACHE_TTL)?,
            admit_negative_cache_ttl: duration_env(
                "ADMIT_NEGATIVE_CACHE_TTL",
                DEFAULT_ADMIT_NEGATIVE_CACHE_TTL,
            )?,

            rate_limit_auth_failure_requests,
            rate_limit_auth_failure_window_seconds,

            audit_url,
            trusted_proxies,
        })
    }
}

/// A duration as the config writes one: a whole number and `s`, `m`, `h` or `d`.
pub fn parse_duration(raw: &str) -> Option<Duration> {
    let raw = raw.trim();
    let split = raw.find(|c: char| !c.is_ascii_digit())?;
    let (number, unit) = raw.split_at(split);
    let n: u64 = number.parse().ok()?;
    let secs = |mult: u64| n.checked_mul(mult).map(Duration::from_secs);
    match unit {
        "s" => secs(1),
        "m" => secs(60),
        "h" => secs(3600),
        "d" => secs(86_400),
        _ => None,
    }
}

/// `*` accepts any audience; otherwise a comma-separated list. None when empty.
fn parse_audiences(raw: &str) -> Option<Audiences> {
    if raw.trim() == "*" {
        return Some(Audiences::Any);
    }
    let list: Vec<String> = raw
        .split(',')
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
        .collect();
    (!list.is_empty()).then_some(Audiences::OneOf(list))
}

fn base_url(raw: String) -> String {
    raw.trim().trim_end_matches('/').to_string()
}

fn require_env(key: &'static str) -> Result<String, GatewayConfigError> {
    env::var(key).map_err(|_| GatewayConfigError::Missing(key))
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

fn duration_env(key: &'static str, default: Duration) -> Result<Duration, GatewayConfigError> {
    match env::var(key) {
        Ok(raw) => parse_duration(&raw).ok_or_else(|| GatewayConfigError::Invalid {
            key,
            message: format!("{raw:?} is not a duration such as 10s or 5m"),
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

    #[test]
    fn durations_are_a_number_and_a_unit() {
        assert_eq!(parse_duration("10s"), Some(Duration::from_secs(10)));
        assert_eq!(parse_duration("5m"), Some(Duration::from_secs(300)));
        assert_eq!(parse_duration("2h"), Some(Duration::from_secs(7200)));
        assert_eq!(parse_duration("30d"), Some(Duration::from_secs(2_592_000)));
        for bad in ["", "10", "s", "1.5s", "10 s", "-1s", "10w", "500ms"] {
            assert_eq!(parse_duration(bad), None, "{bad}");
        }
    }

    #[test]
    fn audiences_are_a_list_or_any() {
        assert_eq!(parse_audiences("*"), Some(Audiences::Any));
        assert_eq!(
            parse_audiences("api, console"),
            Some(Audiences::OneOf(vec!["api".into(), "console".into()]))
        );
        assert_eq!(parse_audiences(" , "), None);
    }
}
