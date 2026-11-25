//! Authorizer configuration DTOs and snapshot parsing.

use std::collections::BTreeMap;
use std::time::Duration;

use serde::Deserialize;

use crate::duration::parse_duration;
use crate::route_map::Upstream;

/// Raw authorizer entry from the control plane's gateway feed (`authorizers.<name>`).
#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
pub struct AuthorizerDoc {
    pub url: String,
    pub timeout: String,
    pub cache_ttl: String,
    pub negative_cache_ttl: String,
    #[serde(default)]
    pub token_env: Option<String>,
}

/// Compiled authorizer configuration inside a loaded gateway snapshot.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct AuthorizerSpec {
    pub name: String,
    pub url: String,
    pub timeout: Duration,
    pub cache_ttl: Duration,
    pub negative_cache_ttl: Duration,
    /// Resolved bearer token from `token_env`, if configured.
    pub token: Option<String>,
}

/// How an external authorizer combines with Plat5's built-in RBAC.
#[derive(Clone, Copy, Debug, Default, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub enum AuthorizerMode {
    #[default]
    Replace,
    And,
    Or,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
pub struct RouteResourceDoc {
    #[serde(rename = "type")]
    pub r#type: String,
    pub id: String,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
pub struct RouteAuthorizerDoc {
    pub name: String,
    #[serde(default)]
    pub mode: AuthorizerMode,
    #[serde(default)]
    pub resource: Option<RouteResourceDoc>,
    #[serde(default)]
    pub action: Option<String>,
}

#[derive(Debug)]
pub struct RouteResource {
    pub r#type: String,
    pub id: Upstream,
}

#[derive(Debug)]
pub struct RouteAuthorizer {
    pub name: String,
    pub mode: AuthorizerMode,
    pub resource: Option<RouteResource>,
    pub action: Option<String>,
}

/// Parse and validate all `authorizers` in a control-plane snapshot, resolving
/// any referenced `token_env` from the process environment.
pub fn parse_authorizers(
    docs: &BTreeMap<String, AuthorizerDoc>,
) -> Result<BTreeMap<String, AuthorizerSpec>, String> {
    parse_authorizers_with_env(docs, |k| std::env::var(k).ok())
}

pub fn parse_authorizers_with_env(
    docs: &BTreeMap<String, AuthorizerDoc>,
    lookup_env: impl Fn(&str) -> Option<String>,
) -> Result<BTreeMap<String, AuthorizerSpec>, String> {
    let mut out = BTreeMap::new();
    for (name, doc) in docs {
        let timeout = parse_duration(&doc.timeout)
            .filter(|d| !d.is_zero())
            .ok_or_else(|| {
                format!(
                    "authorizers.{name}.timeout {:?} is invalid (must be >= 1s)",
                    doc.timeout
                )
            })?;
        let cache_ttl = parse_duration(&doc.cache_ttl).ok_or_else(|| {
            format!(
                "authorizers.{name}.cache_ttl {:?} is invalid",
                doc.cache_ttl
            )
        })?;
        let negative_cache_ttl = parse_duration(&doc.negative_cache_ttl).ok_or_else(|| {
            format!(
                "authorizers.{name}.negative_cache_ttl {:?} is invalid",
                doc.negative_cache_ttl
            )
        })?;
        let url = doc.url.trim().trim_end_matches('/').to_string();
        if !(url.starts_with("http://") || url.starts_with("https://")) {
            return Err(format!(
                "authorizers.{name}.url {:?} must be http or https",
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
                        format!(
                            "authorizers.{name}.token_env references {env_name}, which is not set"
                        )
                    })?;
                Some(val)
            }
        };
        out.insert(
            name.clone(),
            AuthorizerSpec {
                name: name.clone(),
                url,
                timeout,
                cache_ttl,
                negative_cache_ttl,
                token,
            },
        );
    }
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_authorizers_resolves_token_env_and_refuses_missing() {
        let mut docs = BTreeMap::new();
        docs.insert(
            "topaz".into(),
            AuthorizerDoc {
                url: "http://topaz:8383/".into(),
                timeout: "2s".into(),
                cache_ttl: "0s".into(),
                negative_cache_ttl: "5s".into(),
                token_env: Some("TOPAZ_TOKEN".into()),
            },
        );
        let ok = parse_authorizers_with_env(&docs, |k| {
            (k == "TOPAZ_TOKEN").then(|| "secret-123".to_string())
        })
        .unwrap();
        assert_eq!(ok["topaz"].url, "http://topaz:8383");
        assert_eq!(ok["topaz"].token.as_deref(), Some("secret-123"));
        assert_eq!(ok["topaz"].cache_ttl, Duration::ZERO);
        assert_eq!(ok["topaz"].negative_cache_ttl, Duration::from_secs(5));

        let err = parse_authorizers_with_env(&docs, |_| None).unwrap_err();
        assert!(err.contains("TOPAZ_TOKEN"), "{err}");
    }
}
