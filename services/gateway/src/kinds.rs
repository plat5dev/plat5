//! The deployment's kinds, as the gateway uses them: each kind's chain of
//! ancestors, for a route's node chain (docs/reference/config/kinds.md).

use std::collections::{BTreeMap, HashMap};

use serde::Deserialize;

/// One kind as the feed sends it. Fields the gateway doesn't use are ignored.
#[derive(Clone, Debug, Deserialize)]
pub struct KindDoc {
    #[serde(default)]
    pub parent: Option<String>,
    #[serde(default)]
    pub path: Option<String>,
    #[serde(default)]
    pub host: Option<String>,
}

/// Host-based root resolution for a root kind: `host: "{<kind>}.<domain>"`
/// paired with `path: "<prefix>/{<kind>}"`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct RootHost {
    pub kind: String,
    /// Path prefix before `/{<kind>}`, e.g. `"/orgs"`.
    pub prefix: String,
    /// Lowercased domain suffix including the leading dot, e.g. `".api.example.com"`.
    pub suffix: String,
}

#[derive(Clone, Debug)]
pub struct Kinds {
    parents: HashMap<String, Option<String>>,
    host_routes: Vec<RootHost>,
}

impl Kinds {
    pub fn from_feed(kinds: &BTreeMap<String, KindDoc>) -> Self {
        let mut host_routes = Vec::new();
        for (name, kind) in kinds {
            if kind.parent.is_some() {
                continue;
            }
            let (Some(host), Some(path)) = (kind.host.as_deref(), kind.path.as_deref()) else {
                continue;
            };
            let param = format!("{{{name}}}");
            let Some(domain) = host.strip_prefix(&format!("{param}.")) else {
                continue;
            };
            if domain.is_empty() {
                continue;
            }
            let Some(prefix) = path.strip_suffix(&format!("/{param}")) else {
                continue;
            };
            host_routes.push(RootHost {
                kind: name.clone(),
                prefix: prefix.to_string(),
                suffix: format!(".{}", domain.to_ascii_lowercase()),
            });
        }
        Self {
            parents: kinds
                .iter()
                .map(|(name, kind)| (name.clone(), kind.parent.clone()))
                .collect(),
            host_routes,
        }
    }

    pub fn host_routes(&self) -> &[RootHost] {
        &self.host_routes
    }

    /// `kind` and its ancestors, root first. Err when the parents don't lead to a root.
    pub fn chain(&self, kind: &str) -> Result<Vec<String>, String> {
        let mut chain = vec![kind.to_string()];
        let mut at = kind;
        loop {
            let Some(parent) = self.parents.get(at) else {
                return Err(format!("{at} is not a kind"));
            };
            let Some(parent) = parent.as_deref() else {
                chain.reverse();
                return Ok(chain);
            };
            if chain.len() > self.parents.len() {
                return Err(format!("{kind}'s parents never reach a root"));
            }
            chain.push(parent.to_string());
            at = parent;
        }
    }
}

/// Match a request `Host` header (optional `:port`) against configured root
/// host templates. Returns the matched [`RootHost`] and the extracted root ref
/// (single DNS label, lowercased).
pub fn resolve_host<'a>(
    routes: &'a [RootHost],
    host_header: &str,
) -> Option<(&'a RootHost, String)> {
    let host = strip_port(host_header.trim()).to_ascii_lowercase();
    for route in routes {
        let Some(label) = host.strip_suffix(&route.suffix) else {
            continue;
        };
        if is_valid_host_label(label) {
            return Some((route, label.to_string()));
        }
    }
    None
}

fn strip_port(host: &str) -> &str {
    // IPv6 literals are bracketed (`[::1]:8080`) and never match a domain suffix.
    if host.starts_with('[') {
        return host;
    }
    match host.rsplit_once(':') {
        Some((h, port)) if !port.is_empty() && port.bytes().all(|b| b.is_ascii_digit()) => h,
        _ => host,
    }
}

fn is_valid_host_label(label: &str) -> bool {
    !label.is_empty()
        && label.len() <= 63
        && !label.starts_with('-')
        && !label.ends_with('-')
        && label
            .bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-')
}

/// `X-Plat5-<Kind>-Id`: the kind in title case, `_` as `-` (docs/reference/headers.md).
pub fn id_header(kind: &str) -> String {
    let title: Vec<String> = kind
        .split('_')
        .map(|part| {
            let mut chars = part.chars();
            match chars.next() {
                Some(c) => c.to_ascii_uppercase().to_string() + chars.as_str(),
                None => String::new(),
            }
        })
        .collect();
    format!("X-Plat5-{}-Id", title.join("-"))
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn kinds(v: serde_json::Value) -> Kinds {
        Kinds::from_feed(&serde_json::from_value(v).unwrap())
    }

    #[test]
    fn headers_title_case_each_part() {
        assert_eq!(id_header("project"), "X-Plat5-Project-Id");
        assert_eq!(id_header("cost_center"), "X-Plat5-Cost-Center-Id");
    }

    #[test]
    fn chains_run_from_the_root_down() {
        let k = kinds(json!({
            "organization": { "path": "/orgs/{organization}", "list_permission": "organization:read" },
            "project": { "parent": "organization", "path": "/projects/{project}" },
            "environment": { "parent": "project", "path": "/envs/{environment}" }
        }));
        assert_eq!(k.chain("organization").unwrap(), vec!["organization"]);
        assert_eq!(
            k.chain("environment").unwrap(),
            vec!["organization", "project", "environment"]
        );
        assert!(k.chain("folder").is_err());
    }

    #[test]
    fn parents_that_never_reach_a_root_cannot_build() {
        let k = kinds(json!({
            "a": { "parent": "b" },
            "b": { "parent": "a" },
            "c": { "parent": "missing" }
        }));
        assert!(k.chain("a").is_err());
        assert!(k.chain("c").is_err());
    }

    #[test]
    fn root_host_templates_resolve_single_label_subdomains() {
        let k = kinds(json!({
            "organization": {
                "path": "/orgs/{organization}",
                "host": "{organization}.api.example.com"
            },
            "project": {
                "parent": "organization",
                "path": "/projects/{project}",
                "host": "{project}.ignored.example.com"
            }
        }));
        assert_eq!(k.host_routes().len(), 1);

        let (r, slug) = resolve_host(k.host_routes(), "ACME-1.api.example.com:8443").unwrap();
        assert_eq!(r.kind, "organization");
        assert_eq!(r.prefix, "/orgs");
        assert_eq!(slug, "acme-1");

        // Multi-level subdomains, bare domain, invalid labels and other hosts don't match.
        for host in [
            "api.example.com",
            "a.b.api.example.com",
            "-bad.api.example.com",
            "bad-.api.example.com",
            "acme.other.example.com",
            "acme.ignored.example.com",
        ] {
            assert!(resolve_host(k.host_routes(), host).is_none(), "{host}");
        }
    }
}
