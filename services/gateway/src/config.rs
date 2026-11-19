use std::env;
use std::time::Duration;

use crate::auth::jwt::Audiences;
use crate::auth::{parse_claim_path, parse_subject_claim, NameClaims};
use crate::client_ip::TrustedProxies;
use crate::duration::parse_duration;

const DEFAULT_PORT: &str = "5001";
const DEFAULT_INTERNAL_PORT: &str = "8000";
const DEFAULT_SUBJECT_CLAIM: &str = "sub";
const DEFAULT_NAME_CLAIMS: &str = "name,preferred_username";
const DEFAULT_ADMIT_CACHE_TTL: Duration = Duration::from_secs(5 * 60);
const DEFAULT_ADMIT_NEGATIVE_CACHE_TTL: Duration = Duration::from_secs(10);
const DEFAULT_RATE_LIMIT_AUTH_FAILURE_REQUESTS: u64 = 60;
const DEFAULT_RATE_LIMIT_AUTH_FAILURE_WINDOW: Duration = Duration::from_secs(60);

/// Process configuration loaded from environment (docs/reference/env.md#gateway).
#[derive(Clone, Debug)]
pub struct GatewayConfig {
    pub port: String,
    pub internal_port: String,
    /// The control plane's internal base URL (`CONTROL_PLANE_URL`): kinds, routes,
    /// rate limits and CORS come from `{url}/internal/gateway`.
    pub control_plane_url: String,
    /// Valkey for rate-limit counters. None = rate limiting off (`RATE_LIMIT_ENABLED=false`).
    pub valkey_url: Option<String>,

    pub auth_issuer: String,
    pub auth_jwks_uri: String,
    pub auth_allowed_audiences: Audiences,
    /// Dotted claim path segments for the person's IdP subject (`AUTH_SUBJECT_CLAIM`).
    pub auth_subject_claim: Vec<String>,
    /// Dotted claim paths checked in order to seed a person's name on first sign-in (`AUTH_NAME_CLAIMS`).
    pub auth_name_claims: NameClaims,

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
        let auth_name_claims = parse_name_claims(
            &env::var("AUTH_NAME_CLAIMS").unwrap_or_else(|_| DEFAULT_NAME_CLAIMS.to_string()),
        );
        let identity_internal_url = base_url(require_env("IDENTITY_INTERNAL_URL")?);
        let control_plane_url = base_url(require_env("CONTROL_PLANE_URL")?);
        let valkey_url = optional_module_url(
            "RATE_LIMIT_ENABLED",
            "VALKEY_URL",
            env::var("RATE_LIMIT_ENABLED").ok().as_deref(),
            env::var("VALKEY_URL").ok().as_deref(),
            false,
        )?;
        let audit_url = optional_module_url(
            "AUDIT_ENABLED",
            "AUDIT_URL",
            env::var("AUDIT_ENABLED").ok().as_deref(),
            env::var("AUDIT_URL").ok().as_deref(),
            true,
        )?;
        let rate_limit_auth_failure_requests = parse_u64_env(
            "RATE_LIMIT_AUTH_FAILURE_REQUESTS",
            DEFAULT_RATE_LIMIT_AUTH_FAILURE_REQUESTS,
        )?;
        let rate_limit_auth_failure_window = positive_duration_env(
            "RATE_LIMIT_AUTH_FAILURE_WINDOW",
            DEFAULT_RATE_LIMIT_AUTH_FAILURE_WINDOW,
        )?;

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
            auth_name_claims,

            identity_internal_url,

            admit_cache_ttl: duration_env("ADMIT_CACHE_TTL", DEFAULT_ADMIT_CACHE_TTL)?,
            admit_negative_cache_ttl: duration_env(
                "ADMIT_NEGATIVE_CACHE_TTL",
                DEFAULT_ADMIT_NEGATIVE_CACHE_TTL,
            )?,

            rate_limit_auth_failure_requests,
            rate_limit_auth_failure_window_seconds: rate_limit_auth_failure_window.as_secs(),

            audit_url,
            trusted_proxies,
        })
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

fn parse_name_claims(raw: &str) -> NameClaims {
    raw.split(',')
        .map(str::trim)
        .filter(|s| !s.is_empty())
        .map(parse_claim_path)
        .filter(|p| !p.is_empty())
        .collect()
}

fn base_url(raw: String) -> String {
    raw.trim().trim_end_matches('/').to_string()
}

fn require_env(key: &'static str) -> Result<String, GatewayConfigError> {
    require_non_empty(key, env::var(key))
}

fn require_non_empty(
    key: &'static str,
    raw: Result<String, env::VarError>,
) -> Result<String, GatewayConfigError> {
    let val = raw.map_err(|_| GatewayConfigError::Missing(key))?;
    let trimmed = val.trim();
    if trimmed.is_empty() {
        return Err(GatewayConfigError::Invalid {
            key,
            message: "must not be empty".to_string(),
        });
    }
    Ok(trimmed.to_string())
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

fn positive_duration_env(
    key: &'static str,
    default: Duration,
) -> Result<Duration, GatewayConfigError> {
    match env::var(key) {
        Ok(raw) => parse_positive_duration(key, &raw),
        Err(_) => Ok(default),
    }
}

fn parse_positive_duration(key: &'static str, raw: &str) -> Result<Duration, GatewayConfigError> {
    let duration = parse_duration(raw).ok_or_else(|| GatewayConfigError::Invalid {
        key,
        message: format!("{raw:?} is not a duration such as 10s or 5m"),
    })?;
    if duration < Duration::from_secs(1) {
        return Err(GatewayConfigError::Invalid {
            key,
            message: "must be at least 1s".to_string(),
        });
    }
    Ok(duration)
}

/// Fail-closed module switch (`AUDIT_ENABLED` / `RATE_LIMIT_ENABLED`): on unless
/// explicitly `"false"`. When on, its backing URL is required so a forgotten
/// URL fails boot instead of silently disabling the module.
fn optional_module_url(
    enabled_key: &'static str,
    url_key: &'static str,
    enabled: Option<&str>,
    url: Option<&str>,
    strip_trailing_slash: bool,
) -> Result<Option<String>, GatewayConfigError> {
    match enabled.map(str::trim) {
        None | Some("") | Some("true") => {}
        Some("false") => return Ok(None),
        Some(other) => {
            return Err(GatewayConfigError::Invalid {
                key: enabled_key,
                message: format!("must be true or false, got {other:?}"),
            })
        }
    }
    match url.map(str::trim) {
        Some(u) if !u.is_empty() => Ok(Some(if strip_trailing_slash {
            u.trim_end_matches('/').to_string()
        } else {
            u.to_string()
        })),
        _ => Err(GatewayConfigError::Missing(url_key)),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn optional_modules_fail_closed_when_enabled() {
        assert_eq!(
            optional_module_url(
                "AUDIT_ENABLED",
                "AUDIT_URL",
                None,
                Some("http://audit:3003/"),
                true
            )
            .unwrap()
            .as_deref(),
            Some("http://audit:3003")
        );
        assert!(matches!(
            optional_module_url("RATE_LIMIT_ENABLED", "VALKEY_URL", None, None, false),
            Err(GatewayConfigError::Missing("VALKEY_URL"))
        ));
        assert_eq!(
            optional_module_url(
                "RATE_LIMIT_ENABLED",
                "VALKEY_URL",
                Some("false"),
                None,
                false
            )
            .unwrap(),
            None
        );
        assert!(optional_module_url(
            "AUDIT_ENABLED",
            "AUDIT_URL",
            Some("no"),
            Some("http://audit:3003"),
            true
        )
        .is_err());
    }

    #[test]
    fn audiences_and_name_claims_parse() {
        assert_eq!(parse_audiences("*"), Some(Audiences::Any));
        assert_eq!(
            parse_audiences("api, console"),
            Some(Audiences::OneOf(vec!["api".into(), "console".into()]))
        );
        assert_eq!(parse_audiences(" , "), None);
        assert_eq!(
            parse_name_claims("profile.display_name, name"),
            vec![vec!["profile", "display_name"], vec!["name"]]
        );
    }

    #[test]
    fn require_non_empty_rejects_missing_empty_and_whitespace() {
        assert!(matches!(
            require_non_empty("AUTH_ISSUER", Err(env::VarError::NotPresent)),
            Err(GatewayConfigError::Missing("AUTH_ISSUER"))
        ));
        for raw in ["", "   ", "\t\n"] {
            assert!(
                matches!(
                    require_non_empty("AUTH_ISSUER", Ok(raw.to_string())),
                    Err(GatewayConfigError::Invalid {
                        key: "AUTH_ISSUER",
                        ..
                    })
                ),
                "accepted {raw:?}"
            );
        }
        assert_eq!(
            require_non_empty("AUTH_ISSUER", Ok("  https://auth.example.com  ".into())).unwrap(),
            "https://auth.example.com"
        );
    }

    #[test]
    fn rate_limit_window_rejects_zero_duration() {
        for raw in ["0s", "0m", "0h", "0d"] {
            assert!(
                matches!(
                    parse_positive_duration("RATE_LIMIT_AUTH_FAILURE_WINDOW", raw),
                    Err(GatewayConfigError::Invalid {
                        key: "RATE_LIMIT_AUTH_FAILURE_WINDOW",
                        ..
                    })
                ),
                "accepted {raw:?}"
            );
        }
        assert_eq!(
            parse_positive_duration("RATE_LIMIT_AUTH_FAILURE_WINDOW", "1s").unwrap(),
            Duration::from_secs(1)
        );
    }
}
