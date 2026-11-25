//! Authorizer configuration DTOs and snapshot parsing.

use std::time::Duration;

use serde::Deserialize;

use crate::duration::parse_duration;
use crate::route_map::Upstream;

/// Raw top-level `authorizer` entry from the control plane's gateway feed.
#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
pub struct AuthorizerDoc {
    pub url: String,
    pub timeout: String,
    pub cache_ttl: String,
    pub negative_cache_ttl: String,
    #[serde(default)]
    pub token_env: Option<String>,
}

/// Compiled deployment-wide authorizer inside a loaded gateway snapshot.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct AuthorizerSpec {
    pub url: String,
    pub timeout: Duration,
    pub cache_ttl: Duration,
    pub negative_cache_ttl: Duration,
    /// Resolved bearer token from `token_env`, if configured.
    pub token: Option<String>,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
pub struct RouteResourceDoc {
    #[serde(rename = "type")]
    pub r#type: String,
    pub id: String,
}

#[derive(Debug)]
pub struct RouteResource {
    pub r#type: String,
    pub id: Upstream,
}

/// Parse and validate the optional top-level `authorizer` in a control-plane
/// snapshot, resolving any referenced `token_env` from the process environment.
pub fn parse_authorizer(doc: Option<&AuthorizerDoc>) -> Result<Option<AuthorizerSpec>, String> {
    parse_authorizer_with_env(doc, |k| std::env::var(k).ok())
}

pub fn parse_authorizer_with_env(
    doc: Option<&AuthorizerDoc>,
    lookup_env: impl Fn(&str) -> Option<String>,
) -> Result<Option<AuthorizerSpec>, String> {
    let Some(doc) = doc else {
        return Ok(None);
    };
    let timeout = parse_duration(&doc.timeout)
        .filter(|d| !d.is_zero())
        .ok_or_else(|| {
            format!(
                "authorizer.timeout {:?} is invalid (must be >= 1s)",
                doc.timeout
            )
        })?;
    let cache_ttl = parse_duration(&doc.cache_ttl)
        .ok_or_else(|| format!("authorizer.cache_ttl {:?} is invalid", doc.cache_ttl))?;
    let negative_cache_ttl = parse_duration(&doc.negative_cache_ttl).ok_or_else(|| {
        format!(
            "authorizer.negative_cache_ttl {:?} is invalid",
            doc.negative_cache_ttl
        )
    })?;
    let url = doc.url.trim().trim_end_matches('/').to_string();
    if !(url.starts_with("http://") || url.starts_with("https://")) {
        return Err(format!(
            "authorizer.url {:?} must be http or https",
            doc.url
        ));
    }
    let token = match &doc.token_env {
        None => None,
        Some(env_name) => {
            let val = lookup_env(env_name)
                .map(|v| v.trim().to_string())
                .filter(|v| !v.is_empty())
                .ok_or_else(|| {
                    format!("authorizer.token_env references {env_name}, which is not set")
                })?;
            Some(val)
        }
    };
    Ok(Some(AuthorizerSpec {
        url,
        timeout,
        cache_ttl,
        negative_cache_ttl,
        token,
    }))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_authorizer_resolves_token_env_and_refuses_missing() {
        let doc = AuthorizerDoc {
            url: "http://topaz:8383/".into(),
            timeout: "2s".into(),
            cache_ttl: "0s".into(),
            negative_cache_ttl: "5s".into(),
            token_env: Some("TOPAZ_TOKEN".into()),
        };
        let ok = parse_authorizer_with_env(Some(&doc), |k| {
            (k == "TOPAZ_TOKEN").then(|| "secret-123".to_string())
        })
        .unwrap()
        .unwrap();
        assert_eq!(ok.url, "http://topaz:8383");
        assert_eq!(ok.token.as_deref(), Some("secret-123"));
        assert_eq!(ok.cache_ttl, Duration::ZERO);
        assert_eq!(ok.negative_cache_ttl, Duration::from_secs(5));

        let err = parse_authorizer_with_env(Some(&doc), |_| None).unwrap_err();
        assert!(err.contains("TOPAZ_TOKEN"), "{err}");

        assert!(parse_authorizer_with_env(None, |_| None).unwrap().is_none());
    }
}
