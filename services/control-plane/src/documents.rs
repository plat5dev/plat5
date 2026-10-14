//! Deployment-wide documents other than routes. Each kind is a typed schema
//! with its own validation; there is no free-form key. Contract:
//! docs/control-plane.md.

use std::collections::{BTreeMap, HashSet};

use serde::de::DeserializeOwned;
use serde::{Deserialize, Serialize};

use crate::route_config::valid_scope_label;

/// A document kind: its path segment, its stored key, and what makes it valid.
pub trait Document: Serialize + DeserializeOwned + Send + Sync + 'static {
    /// The stored kind and the admin path (`/roles`, `/cors`).
    const KIND: &'static str;
    fn validate(&self) -> Result<(), String>;
}

/// Wildcard role grant: every label. Valid only alone.
pub const WILDCARD: &str = "*";
/// Most labels one role may grant.
pub const MAX_ROLE_LABELS: usize = 64;

/// The deployment's roles: slug → labels. Identity reads it; the gateway never
/// does. Same rules as identity's parser (services/identity/roles).
#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct Roles {
    /// `None` is a role written with no list (`member:`), which is refused.
    #[serde(default)]
    pub roles: BTreeMap<String, Option<Vec<String>>>,
    #[serde(default)]
    pub creator_role: String,
    #[serde(default)]
    pub default_role: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub service_account_default_role: Option<String>,
}

impl Document for Roles {
    const KIND: &'static str = "roles";

    fn validate(&self) -> Result<(), String> {
        if self.roles.is_empty() {
            return Err("roles must define at least one role".into());
        }
        for (slug, labels) in &self.roles {
            if !valid_scope_label(slug) {
                return Err(format!("role {slug:?}: slug must be [a-z0-9:._-]+, max 64"));
            }
            validate_role_labels(labels.as_deref()).map_err(|e| format!("role {slug:?}: {e}"))?;
        }
        let is_role = |field: &str, value: &str| -> Result<(), String> {
            if value.is_empty() {
                return Err(format!("{field} is required"));
            }
            if !self.roles.contains_key(value) {
                return Err(format!("{field} {value:?} is not a role"));
            }
            Ok(())
        };
        is_role("creator_role", &self.creator_role)?;
        is_role("default_role", &self.default_role)?;
        if let Some(sa) = &self.service_account_default_role {
            is_role("service_account_default_role", sa)?;
        }
        Ok(())
    }
}

fn validate_role_labels(labels: Option<&[String]>) -> Result<(), String> {
    let Some(labels) = labels else {
        return Err(r#"labels are missing; use [] for none or ["*"] for every label"#.into());
    };
    if labels.len() == 1 && labels[0] == WILDCARD {
        return Ok(());
    }
    if labels.len() > MAX_ROLE_LABELS {
        return Err(format!("more than {MAX_ROLE_LABELS} labels"));
    }
    let mut seen = HashSet::new();
    for label in labels {
        if label == WILDCARD {
            return Err(r#""*" is only valid alone"#.into());
        }
        if !valid_scope_label(label) {
            return Err(format!("label {label:?} must be [a-z0-9:._-]+, max 64"));
        }
        if !seen.insert(label) {
            return Err(format!("label {label:?} is repeated"));
        }
    }
    Ok(())
}

/// Most origins a CORS document may list.
pub const MAX_ORIGINS: usize = 100;

/// Browser origins the gateway answers CORS for. Empty allows any origin (`*`).
#[derive(Clone, Debug, Default, Deserialize, Serialize, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct Cors {
    #[serde(default)]
    pub allowed_origins: Vec<String>,
}

impl Document for Cors {
    const KIND: &'static str = "cors";

    fn validate(&self) -> Result<(), String> {
        if self.allowed_origins.len() > MAX_ORIGINS {
            return Err(format!("more than {MAX_ORIGINS} allowed_origins"));
        }
        let mut seen = HashSet::new();
        for origin in &self.allowed_origins {
            validate_origin(origin)?;
            if !seen.insert(origin) {
                return Err(format!("origin {origin:?} is repeated"));
            }
        }
        Ok(())
    }
}

const ORIGIN_FORM: &str =
    "expected scheme://host or scheme://host:port, e.g. https://app.example.com";

/// An origin is compared byte for byte with the browser's `Origin` header, so
/// it must be in the form the browser sends: lowercase, no path, no trailing `/`.
fn validate_origin(origin: &str) -> Result<(), String> {
    let bad = |why: &str| Err(format!("origin {origin:?}: {why} ({ORIGIN_FORM})"));
    let rest = if let Some(r) = origin.strip_prefix("https://") {
        r
    } else if let Some(r) = origin.strip_prefix("http://") {
        r
    } else {
        return bad("must start with https:// or http://");
    };
    if rest.is_empty() {
        return bad("needs a host");
    }
    if rest.contains(['/', '?', '#', '@', '*']) || rest.contains(char::is_whitespace) {
        return bad("must be only scheme, host, and port");
    }
    if rest.chars().any(|c| c.is_ascii_uppercase()) {
        return bad("must be lowercase");
    }
    if let Some((host, port)) = rest.rsplit_once(':') {
        if host.is_empty() {
            return bad("needs a host");
        }
        if !port.parse::<u32>().is_ok_and(|p| (1..=65535).contains(&p)) {
            return bad("port must be 1-65535");
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    const STARTER: &str = r#"
roles:
  owner: ["*"]
  admin: [org:write, org:members:write, org:service-accounts:write, org:audit:read]
  member: []
creator_role: owner
default_role: member
"#;

    fn roles(yaml: &str) -> Result<Roles, String> {
        let doc: Roles = serde_yaml::from_str(yaml).map_err(|e| e.to_string())?;
        doc.validate()?;
        Ok(doc)
    }

    #[test]
    fn starter_roles_are_valid() {
        let doc = roles(STARTER).unwrap();
        assert_eq!(doc.roles["owner"], Some(vec!["*".to_string()]));
        assert_eq!(doc.roles["member"], Some(vec![]));
        assert_eq!(doc.service_account_default_role, None);
    }

    #[test]
    fn roles_refuse_what_identity_refuses() {
        let cases = [
            (
                "roles: {}\ncreator_role: a\ndefault_role: a",
                "at least one role",
            ),
            (
                "roles: {a: []}\ndefault_role: a",
                "creator_role is required",
            ),
            (
                "roles: {a: []}\ncreator_role: a",
                "default_role is required",
            ),
            (
                "roles: {a: []}\ncreator_role: b\ndefault_role: a",
                "creator_role \"b\" is not a role",
            ),
            (
                "roles: {a: []}\ncreator_role: a\ndefault_role: a\nservice_account_default_role: b",
                "service_account_default_role \"b\" is not a role",
            ),
            (
                "roles: {A: []}\ncreator_role: A\ndefault_role: A",
                "slug must be",
            ),
            (
                "roles: {a: }\ncreator_role: a\ndefault_role: a",
                "labels are missing",
            ),
            (
                "roles: {a: ['*', x]}\ncreator_role: a\ndefault_role: a",
                "only valid alone",
            ),
            (
                "roles: {a: [x, x]}\ncreator_role: a\ndefault_role: a",
                "repeated",
            ),
            (
                "roles: {a: ['X']}\ncreator_role: a\ndefault_role: a",
                "must be [a-z0-9:._-]+",
            ),
        ];
        for (yaml, want) in cases {
            let err = roles(yaml).unwrap_err();
            assert!(err.contains(want), "{yaml}: {err}");
        }
        let many: Vec<String> = (0..=MAX_ROLE_LABELS).map(|i| format!("l{i}")).collect();
        let doc = Roles {
            roles: BTreeMap::from([("a".to_string(), Some(many))]),
            creator_role: "a".into(),
            default_role: "a".into(),
            service_account_default_role: None,
        };
        assert!(doc.validate().unwrap_err().contains("more than 64 labels"));
    }

    #[test]
    fn roles_refuse_unknown_keys() {
        let err = roles("roles: {a: []}\ncreator_role: a\ndefault_role: a\nextra: 1").unwrap_err();
        assert!(err.contains("unknown field"), "{err}");
    }

    #[test]
    fn roles_round_trip_as_written() {
        let doc = roles(STARTER).unwrap();
        let json = serde_json::to_value(&doc).unwrap();
        assert_eq!(json["roles"]["owner"], serde_json::json!(["*"]));
        assert!(json.get("service_account_default_role").is_none());
        let back: Roles = serde_json::from_value(json).unwrap();
        assert_eq!(back, doc);
    }

    #[test]
    fn cors_origins_are_browser_form() {
        let ok = Cors {
            allowed_origins: vec![
                "https://app.example.com".into(),
                "http://localhost:5173".into(),
            ],
        };
        ok.validate().unwrap();
        Cors::default().validate().unwrap();

        for bad in [
            "app.example.com",
            "https://app.example.com/",
            "https://app.example.com/path",
            "https://App.example.com",
            "https://*.example.com",
            "https://app.example.com:0",
            "https://app.example.com:x",
            "ftp://app.example.com",
            "https://",
            "null",
        ] {
            let doc = Cors {
                allowed_origins: vec![bad.into()],
            };
            assert!(doc.validate().is_err(), "{bad} should be refused");
        }

        let dup = Cors {
            allowed_origins: vec!["https://a.com".into(), "https://a.com".into()],
        };
        assert!(dup.validate().unwrap_err().contains("repeated"));
    }
}
