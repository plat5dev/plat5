use std::collections::{BTreeMap, HashSet};
use std::path::Path;

use serde::{Deserialize, Serialize};
use serde_json::Value;

use crate::schema;

#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum AccessLevel {
    Read,
    Apply,
}

impl AccessLevel {
    pub fn as_str(self) -> &'static str {
        match self {
            AccessLevel::Read => "read",
            AccessLevel::Apply => "apply",
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, Default)]
#[serde(rename_all = "snake_case")]
pub enum TokenSource {
    #[default]
    AccessToken,
    IdToken,
}

#[derive(Debug, Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct AuthConfig {
    pub issuers: Vec<IssuerConfig>,
}

#[derive(Debug, Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct IssuerConfig {
    pub name: String,
    pub issuer: String,
    #[serde(default)]
    pub jwks_uri: Option<String>,
    pub audiences: Vec<String>,
    #[serde(default = "default_subject_claim")]
    pub subject_claim: String,
    #[serde(default)]
    pub max_token_lifetime: Option<String>,
    #[serde(default)]
    pub cli: Option<CliAuthConfig>,
    pub rules: Vec<AuthRule>,
}

fn default_subject_claim() -> String {
    "sub".to_string()
}

#[derive(Debug, Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct CliAuthConfig {
    pub client_id: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub audience: Option<String>,
    #[serde(default = "default_cli_scopes")]
    pub scopes: Vec<String>,
    #[serde(default)]
    pub token_source: TokenSource,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub authorization_endpoint: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub token_endpoint: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub device_authorization_endpoint: Option<String>,
}

fn default_cli_scopes() -> Vec<String> {
    vec!["openid".to_string()]
}

#[derive(Debug, Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct AuthRule {
    pub access: AccessLevel,
    pub claims: BTreeMap<String, Vec<String>>,
}

impl AuthConfig {
    pub fn load_file(path: &Path) -> Result<Self, String> {
        let raw = std::fs::read_to_string(path)
            .map_err(|e| format!("read auth config {}: {e}", path.display()))?;
        Self::parse_yaml_or_json(&raw)
            .map_err(|e| format!("invalid auth config {}: {e}", path.display()))
    }

    pub fn parse_yaml_or_json(raw: &str) -> Result<Self, String> {
        if raw.trim().is_empty() {
            return Err("auth config is empty".to_string());
        }
        let yaml: serde_yaml_ng::Value =
            serde_yaml_ng::from_str(raw).map_err(|e| format!("parse YAML/JSON: {e}"))?;
        let val = serde_json::to_value(yaml).map_err(|e| format!("convert to JSON: {e}"))?;
        if let Err(errs) = schema::check_control_plane_auth(&val) {
            let joined = errs
                .iter()
                .map(|e| e.to_string())
                .collect::<Vec<_>>()
                .join("; ");
            return Err(joined);
        }
        let cfg: AuthConfig =
            serde_json::from_value(val).map_err(|e| format!("decode auth config: {e}"))?;
        cfg.validate_semantics()?;
        Ok(cfg)
    }

    fn validate_semantics(&self) -> Result<(), String> {
        let mut names = HashSet::new();
        let mut issuers = HashSet::new();
        for (i, iss) in self.issuers.iter().enumerate() {
            if !names.insert(iss.name.as_str()) {
                return Err(format!("issuers[{i}].name: duplicate issuer name {:?}", iss.name));
            }
            if !issuers.insert(iss.issuer.as_str()) {
                return Err(format!(
                    "issuers[{i}].issuer: duplicate issuer URL {:?}",
                    iss.issuer
                ));
            }
            if let Some(ref d) = iss.max_token_lifetime {
                parse_duration_secs(d)
                    .ok_or_else(|| format!("issuers[{i}].max_token_lifetime: invalid duration {d:?}"))?;
            }
        }
        Ok(())
    }
}

pub fn parse_duration_secs(s: &str) -> Option<u64> {
    if s.len() < 2 {
        return None;
    }
    let (num, unit) = s.split_at(s.len() - 1);
    let n: u64 = num.parse().ok()?;
    if n == 0 {
        return None;
    }
    let mult: u64 = match unit {
        "s" => 1,
        "m" => 60,
        "h" => 3600,
        "d" => 86_400,
        _ => return None,
    };
    n.checked_mul(mult)
}

/// Walks a dotted JSON path (e.g. `properties.user_id` or `sub`).
pub fn claim_at<'a>(claims: &'a Value, path: &str) -> Option<&'a Value> {
    let mut cur = claims;
    for seg in path.split('.') {
        cur = cur.as_object()?.get(seg)?;
    }
    Some(cur)
}

/// Full-string anchored `*` glob matcher (each `*` matches zero or more chars).
pub fn glob_match(pattern: &str, value: &str) -> bool {
    if !pattern.contains('*') {
        return pattern == value;
    }
    let p: Vec<char> = pattern.chars().collect();
    let v: Vec<char> = value.chars().collect();
    let (mut pi, mut vi) = (0usize, 0usize);
    let mut star_pi: Option<usize> = None;
    let mut star_vi: usize = 0;

    while vi < v.len() {
        if pi < p.len() && p[pi] == '*' {
            star_pi = Some(pi);
            pi += 1;
            star_vi = vi;
        } else if pi < p.len() && p[pi] == v[vi] {
            pi += 1;
            vi += 1;
        } else if let Some(sp) = star_pi {
            pi = sp + 1;
            star_vi += 1;
            vi = star_vi;
        } else {
            return false;
        }
    }
    while pi < p.len() && p[pi] == '*' {
        pi += 1;
    }
    pi == p.len()
}

/// Evaluates whether a single rule's required claims all match the JWT claims.
pub fn rule_matches(rule: &AuthRule, claims: &Value) -> bool {
    if rule.claims.is_empty() {
        return false;
    }
    rule.claims.iter().all(|(path, patterns)| {
        let Some(actual) = claim_at(claims, path) else {
            return false;
        };
        claim_value_matches(path, actual, patterns)
    })
}

fn claim_value_matches(path: &str, actual: &Value, patterns: &[String]) -> bool {
    match actual {
        Value::String(s) => {
            if patterns.iter().any(|p| glob_match(p, s)) {
                return true;
            }
            if matches!(path, "scope" | "scp") {
                return s
                    .split_whitespace()
                    .any(|tok| patterns.iter().any(|p| glob_match(p, tok)));
            }
            false
        }
        Value::Array(items) => items.iter().any(|item| match item {
            Value::String(s) => patterns.iter().any(|p| glob_match(p, s)),
            Value::Bool(b) => {
                let s = b.to_string();
                patterns.iter().any(|p| glob_match(p, &s))
            }
            Value::Number(n) => {
                let s = n.to_string();
                patterns.iter().any(|p| glob_match(p, &s))
            }
            _ => false,
        }),
        Value::Bool(b) => {
            let s = b.to_string();
            patterns.iter().any(|p| glob_match(p, &s))
        }
        Value::Number(n) => {
            let s = n.to_string();
            patterns.iter().any(|p| glob_match(p, &s))
        }
        _ => false,
    }
}
