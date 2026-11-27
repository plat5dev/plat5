use std::env;
use std::time::Duration;

use crate::auth::jwt::Audiences;
use crate::auth::{parse_claim_path, parse_subject_claim, NameClaims};
use crate::client_ip::TrustedProxies;
use crate::duration::parse_duration;
use crate::gateway::signing::UpstreamSigner;
use crate::rate_limit::DEFAULT_KEY_PREFIX;

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
    /// and rate limits come from `{url}/internal/gateway`.
    pub control_plane_url: String,
    /// Valkey for rate-limit counters. None = rate limiting off (`RATE_LIMIT_ENABLED=false`).
    pub valkey_url: Option<String>,
    /// Key prefix for Valkey rate-limit counters (`VALKEY_KEY_PREFIX`).
    pub valkey_key_prefix: String,

    pub auth_issuer: String,
    pub auth_jwks_uri: String,
    pub auth_allowed_audiences: Audiences,
    /// Dotted claim path segments for the person's IdP subject (`AUTH_SUBJECT_CLAIM`).
    pub auth_subject_claim: Vec<String>,
    /// Dotted claim paths checked in order to seed a person's name on first sign-in (`AUTH_NAME_CLAIMS`).
    pub auth_name_claims: NameClaims,
    /// Optional dotted claim path for JWT OAuth scopes (`AUTH_SCOPE_CLAIM`).
    pub auth_scope_claim: Option<Vec<String>>,

    /// Identity's internal base URL (`IDENTITY_INTERNAL_URL`); admit is at `/internal/admit`.
    pub identity_internal_url: String,

    /// How long an admit answer is kept (`ADMIT_CACHE_TTL`).
    pub admit_cache_ttl: Duration,
    /// How long a refusal is kept (`ADMIT_NEGATIVE_CACHE_TTL`).
    pub admit_negative_cache_ttl: Duration,

    pub rate_limit_auth_failure_requests: u64,
    pub rate_limit_auth_failure_window_seconds: u64,

    /// Audit's internal base URL. None = audit off (`AUDIT_MODE=off`).
    pub audit_url: Option<String>,
    /// Audit delivery mode (`AUDIT_MODE`): `sync` (default), `async`, or `off`.
    pub audit_mode: crate::audit::AuditMode,

    /// Proxies whose `X-Forwarded-For` names the client (`TRUSTED_PROXIES`).
    /// Empty = the TCP peer is the client.
    pub trusted_proxies: TrustedProxies,

    /// Optional Ed25519 signer for outgoing upstream requests (`UPSTREAM_SIGNING_KEY`).
    pub upstream_signer: Option<UpstreamSigner>,

    /// Allowed CORS origins (`CORS_ALLOWED_ORIGINS`). `["*"]` allows any origin; empty allows none.
    pub cors_allowed_origins: Vec<String>,
    /// Extra request headers allowed on CORS preflight (`CORS_ALLOWED_HEADERS`).
    pub cors_allowed_headers: Vec<String>,
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
        let auth_scope_claim = parse_scope_claim(env::var("AUTH_SCOPE_CLAIM").ok().as_deref());
        let identity_internal_url = base_url(require_env("IDENTITY_INTERNAL_URL")?);
        let control_plane_url = base_url(require_env("CONTROL_PLANE_URL")?);
        let valkey_url = optional_module_url(
            "RATE_LIMIT_ENABLED",
            "VALKEY_URL",
            env::var("RATE_LIMIT_ENABLED").ok().as_deref(),
            env::var("VALKEY_URL").ok().as_deref(),
            false,
        )?;
        let (audit_mode, audit_url) = parse_audit_config(
            env::var("AUDIT_MODE").ok().as_deref(),
            env::var("AUDIT_URL").ok().as_deref(),
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
        let valkey_key_prefix = parse_key_prefix(env::var("VALKEY_KEY_PREFIX").ok().as_deref())?;
        let upstream_signer = parse_upstream_signer(
            env::var("UPSTREAM_SIGNING_KEY").ok().as_deref(),
            env::var("UPSTREAM_SIGNING_KEY_ID").ok().as_deref(),
        )?;
        let cors_allowed_origins =
            parse_cors_origins(env::var("CORS_ALLOWED_ORIGINS").ok().as_deref())?;
        let cors_allowed_headers =
            parse_cors_headers(env::var("CORS_ALLOWED_HEADERS").ok().as_deref())?;

        Ok(Self {
            port: env::var("PORT").unwrap_or_else(|_| DEFAULT_PORT.to_string()),
            internal_port: env::var("INTERNAL_PORT")
                .unwrap_or_else(|_| DEFAULT_INTERNAL_PORT.to_string()),
            control_plane_url,
            valkey_url,
            valkey_key_prefix,

            auth_issuer,
            auth_jwks_uri,
            auth_allowed_audiences,
            auth_subject_claim: parse_subject_claim(
                &env::var("AUTH_SUBJECT_CLAIM")
                    .unwrap_or_else(|_| DEFAULT_SUBJECT_CLAIM.to_string()),
            ),
            auth_name_claims,
            auth_scope_claim,

            identity_internal_url,

            admit_cache_ttl: duration_env("ADMIT_CACHE_TTL", DEFAULT_ADMIT_CACHE_TTL)?,
            admit_negative_cache_ttl: duration_env(
                "ADMIT_NEGATIVE_CACHE_TTL",
                DEFAULT_ADMIT_NEGATIVE_CACHE_TTL,
            )?,

            rate_limit_auth_failure_requests,
            rate_limit_auth_failure_window_seconds: rate_limit_auth_failure_window.as_secs(),

            audit_url,
            audit_mode,
            trusted_proxies,
            upstream_signer,
            cors_allowed_origins,
            cors_allowed_headers,
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

fn parse_scope_claim(raw: Option<&str>) -> Option<Vec<String>> {
    let parts = parse_claim_path(raw?);
    (!parts.is_empty()).then_some(parts)
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

/// Fail-closed module switch (`RATE_LIMIT_ENABLED`): on unless
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

fn parse_key_prefix(raw: Option<&str>) -> Result<String, GatewayConfigError> {
    let Some(raw) = raw else {
        return Ok(DEFAULT_KEY_PREFIX.to_string());
    };
    if raw.is_empty() || !raw.bytes().all(|b| b.is_ascii_graphic()) {
        return Err(GatewayConfigError::Invalid {
            key: "VALKEY_KEY_PREFIX",
            message: "must be non-empty visible ASCII without whitespace".to_string(),
        });
    }
    Ok(raw.to_string())
}

/// Parse `AUDIT_MODE` (`sync` | `async` | `off`, default `sync`) and require
/// `AUDIT_URL` unless `AUDIT_MODE=off` so a forgotten URL fails boot closed.
fn parse_audit_config(
    mode_raw: Option<&str>,
    url_raw: Option<&str>,
) -> Result<(crate::audit::AuditMode, Option<String>), GatewayConfigError> {
    let mode = match mode_raw.map(str::trim) {
        None | Some("") | Some("sync") => crate::audit::AuditMode::Sync,
        Some("async") => crate::audit::AuditMode::Async,
        Some("off") => return Ok((crate::audit::AuditMode::Sync, None)),
        Some(other) => {
            return Err(GatewayConfigError::Invalid {
                key: "AUDIT_MODE",
                message: format!("must be sync, async, or off, got {other:?}"),
            })
        }
    };
    match url_raw.map(str::trim) {
        Some(u) if !u.is_empty() => Ok((mode, Some(u.trim_end_matches('/').to_string()))),
        _ => Err(GatewayConfigError::Missing("AUDIT_URL")),
    }
}

const MAX_CORS_ORIGINS: usize = 100;
const MAX_CORS_HEADERS: usize = 32;

/// Parse `CORS_ALLOWED_ORIGINS`: comma-separated origins, or `*` alone.
/// Empty or unset allows no cross-origin requests.
fn parse_cors_origins(raw: Option<&str>) -> Result<Vec<String>, GatewayConfigError> {
    let items: Vec<String> = raw
        .unwrap_or_default()
        .split(',')
        .map(str::trim)
        .filter(|s| !s.is_empty())
        .map(ToString::to_string)
        .collect();
    if items.is_empty() {
        return Ok(Vec::new());
    }
    if items.len() > MAX_CORS_ORIGINS {
        return Err(GatewayConfigError::Invalid {
            key: "CORS_ALLOWED_ORIGINS",
            message: format!("at most {MAX_CORS_ORIGINS} origins allowed"),
        });
    }
    if items.iter().any(|o| o == "*") {
        if items.len() > 1 {
            return Err(GatewayConfigError::Invalid {
                key: "CORS_ALLOWED_ORIGINS",
                message: "\"*\" must appear alone".to_string(),
            });
        }
        return Ok(items);
    }
    let mut seen = std::collections::HashSet::with_capacity(items.len());
    for origin in &items {
        if !is_valid_origin(origin) {
            return Err(GatewayConfigError::Invalid {
                key: "CORS_ALLOWED_ORIGINS",
                message: format!(
                    "origin {origin:?} must be lowercase http://host[:port] or https://host[:port] with no path"
                ),
            });
        }
        if !seen.insert(origin.as_str()) {
            return Err(GatewayConfigError::Invalid {
                key: "CORS_ALLOWED_ORIGINS",
                message: format!("duplicate origin {origin:?}"),
            });
        }
    }
    Ok(items)
}

fn is_valid_origin(origin: &str) -> bool {
    let rest = if let Some(r) = origin.strip_prefix("https://") {
        r
    } else if let Some(r) = origin.strip_prefix("http://") {
        r
    } else {
        return false;
    };
    if rest.is_empty() || rest.contains('/') || rest.contains('?') || rest.contains('#') {
        return false;
    }
    let (host, port) = if let Some(after_open) = rest.strip_prefix('[') {
        let Some((h, tail)) = after_open.split_once(']') else {
            return false;
        };
        if h.is_empty() || h.parse::<std::net::Ipv6Addr>().is_err() {
            return false;
        }
        if tail.is_empty() {
            (h, None)
        } else if let Some(p) = tail.strip_prefix(':') {
            (h, Some(p))
        } else {
            return false;
        }
    } else {
        let (h, p) = match rest.rsplit_once(':') {
            Some((h, p)) => (h, Some(p)),
            None => (rest, None),
        };
        if h.is_empty()
            || !h
                .bytes()
                .all(|b| matches!(b, b'a'..=b'z' | b'0'..=b'9' | b'.' | b'-'))
        {
            return false;
        }
        (h, p)
    };
    if host.is_empty() {
        return false;
    }
    if let Some(p) = port {
        if p.is_empty() || !p.bytes().all(|b| b.is_ascii_digit()) {
            return false;
        }
        if p.parse::<u16>().ok().filter(|&n| n > 0).is_none() {
            return false;
        }
    }
    true
}

/// Parse `CORS_ALLOWED_HEADERS`: comma-separated lowercase HTTP header names.
fn parse_cors_headers(raw: Option<&str>) -> Result<Vec<String>, GatewayConfigError> {
    let items: Vec<String> = raw
        .unwrap_or_default()
        .split(',')
        .map(str::trim)
        .filter(|s| !s.is_empty())
        .map(ToString::to_string)
        .collect();
    if items.len() > MAX_CORS_HEADERS {
        return Err(GatewayConfigError::Invalid {
            key: "CORS_ALLOWED_HEADERS",
            message: format!("at most {MAX_CORS_HEADERS} headers allowed"),
        });
    }
    let mut seen = std::collections::HashSet::with_capacity(items.len());
    for header in &items {
        if header.len() > 128
            || !header
                .bytes()
                .all(|b| matches!(b, b'a'..=b'z' | b'0'..=b'9' | b'-'))
        {
            return Err(GatewayConfigError::Invalid {
                key: "CORS_ALLOWED_HEADERS",
                message: format!("header {header:?} must be lowercase [a-z0-9-]"),
            });
        }
        if !seen.insert(header.as_str()) {
            return Err(GatewayConfigError::Invalid {
                key: "CORS_ALLOWED_HEADERS",
                message: format!("duplicate header {header:?}"),
            });
        }
    }
    Ok(items)
}

fn parse_upstream_signer(
    key_raw: Option<&str>,
    kid_raw: Option<&str>,
) -> Result<Option<UpstreamSigner>, GatewayConfigError> {
    let Some(seed) = key_raw else {
        if kid_raw.is_some() {
            return Err(GatewayConfigError::Invalid {
                key: "UPSTREAM_SIGNING_KEY_ID",
                message: "cannot be set without UPSTREAM_SIGNING_KEY".to_string(),
            });
        }
        return Ok(None);
    };
    if seed.trim().is_empty() {
        return Err(GatewayConfigError::Invalid {
            key: "UPSTREAM_SIGNING_KEY",
            message: "must not be empty when set".to_string(),
        });
    }
    if let Some(kid) = kid_raw {
        if let Err(message) = crate::gateway::signing::validate_kid(kid.trim()) {
            return Err(GatewayConfigError::Invalid {
                key: "UPSTREAM_SIGNING_KEY_ID",
                message,
            });
        }
    }
    UpstreamSigner::from_seed_b64url(seed, kid_raw)
        .map(Some)
        .map_err(|message| GatewayConfigError::Invalid {
            key: "UPSTREAM_SIGNING_KEY",
            message,
        })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn optional_modules_fail_closed_when_enabled() {
        assert_eq!(
            optional_module_url(
                "RATE_LIMIT_ENABLED",
                "VALKEY_URL",
                None,
                Some("redis://valkey:6379/"),
                true
            )
            .unwrap()
            .as_deref(),
            Some("redis://valkey:6379")
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
            "RATE_LIMIT_ENABLED",
            "VALKEY_URL",
            Some("no"),
            Some("redis://valkey:6379"),
            false
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
        assert_eq!(parse_scope_claim(None), None);
        assert_eq!(parse_scope_claim(Some("")), None);
        assert_eq!(parse_scope_claim(Some("  . ")), None);
        assert_eq!(parse_scope_claim(Some("scope")), Some(vec!["scope".into()]));
        assert_eq!(
            parse_scope_claim(Some("auth.scp")),
            Some(vec!["auth".into(), "scp".into()])
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

    #[test]
    fn valkey_key_prefix_defaults_and_validates() {
        assert_eq!(parse_key_prefix(None).unwrap(), DEFAULT_KEY_PREFIX);
        assert_eq!(parse_key_prefix(Some("dep-1:rl:")).unwrap(), "dep-1:rl:");
        assert!(parse_key_prefix(Some("")).is_err());
        assert!(parse_key_prefix(Some("bad prefix")).is_err());
        assert!(parse_key_prefix(Some("bad\t")).is_err());
    }

    #[test]
    fn cors_env_parses_and_validates() {
        assert!(parse_cors_origins(None).unwrap().is_empty());
        assert!(parse_cors_origins(Some("")).unwrap().is_empty());
        assert_eq!(parse_cors_origins(Some("*")).unwrap(), vec!["*"]);
        assert_eq!(
            parse_cors_origins(Some("https://app.example.com, http://localhost:3001")).unwrap(),
            vec!["https://app.example.com", "http://localhost:3001"]
        );
        assert!(parse_cors_origins(Some("*, https://a.com")).is_err());
        assert!(parse_cors_origins(Some("https://a.com/")).is_err());
        assert!(parse_cors_origins(Some("https://A.com")).is_err());
        assert!(parse_cors_origins(Some("https://a.com, https://a.com")).is_err());

        assert!(parse_cors_headers(None).unwrap().is_empty());
        assert_eq!(
            parse_cors_headers(Some("idempotency-key, x-custom")).unwrap(),
            vec!["idempotency-key", "x-custom"]
        );
        assert!(parse_cors_headers(Some("X-Custom")).is_err());
        assert!(parse_cors_headers(Some("x-a, x-a")).is_err());
    }

    #[test]
    fn audit_mode_parses_strictly_and_fails_closed() {
        use crate::audit::AuditMode;
        assert_eq!(
            parse_audit_config(None, Some("http://audit:3003/")).unwrap(),
            (AuditMode::Sync, Some("http://audit:3003".into()))
        );
        assert_eq!(
            parse_audit_config(Some(""), Some("http://audit:3003")).unwrap(),
            (AuditMode::Sync, Some("http://audit:3003".into()))
        );
        assert_eq!(
            parse_audit_config(Some("sync"), Some("http://audit:3003")).unwrap(),
            (AuditMode::Sync, Some("http://audit:3003".into()))
        );
        assert_eq!(
            parse_audit_config(Some("async"), Some("http://audit:3003")).unwrap(),
            (AuditMode::Async, Some("http://audit:3003".into()))
        );
        assert_eq!(parse_audit_config(Some("off"), None).unwrap().1, None);
        assert!(matches!(
            parse_audit_config(None, None),
            Err(GatewayConfigError::Missing("AUDIT_URL"))
        ));
        assert!(matches!(
            parse_audit_config(Some("async"), Some("   ")),
            Err(GatewayConfigError::Missing("AUDIT_URL"))
        ));
        assert!(parse_audit_config(Some("best_effort"), Some("http://audit:3003")).is_err());
    }

    #[test]
    fn upstream_signer_config_fails_closed() {
        use base64::engine::general_purpose::URL_SAFE_NO_PAD;
        use base64::Engine;

        let valid_seed = URL_SAFE_NO_PAD.encode([9u8; 32]);
        assert!(parse_upstream_signer(None, None).unwrap().is_none());
        assert!(parse_upstream_signer(None, Some("k-1")).is_err());
        assert!(parse_upstream_signer(Some(""), None).is_err());
        assert!(parse_upstream_signer(Some("   "), None).is_err());
        assert!(parse_upstream_signer(Some("not-valid"), None).is_err());
        assert!(parse_upstream_signer(Some(&valid_seed), Some("bad kid")).is_err());

        let signer = parse_upstream_signer(Some(&valid_seed), Some("k-2026"))
            .unwrap()
            .unwrap();
        assert_eq!(signer.kid(), "k-2026");

        let default_signer = parse_upstream_signer(Some(&valid_seed), None)
            .unwrap()
            .unwrap();
        assert_eq!(default_signer.kid().len(), 16);
    }
}
