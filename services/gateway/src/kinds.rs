//! The deployment's kinds, as the gateway uses them: each kind's chain of
//! ancestors, for a route's node chain (docs/reference/config/kinds.md).

use std::collections::{BTreeMap, HashMap};

use serde::Deserialize;

/// One kind as the feed sends it. Fields the gateway doesn't use are ignored.
#[derive(Clone, Debug, Deserialize)]
pub struct KindDoc {
    #[serde(default)]
    pub parent: Option<String>,
}

#[derive(Clone, Debug)]
pub struct Kinds {
    parents: HashMap<String, Option<String>>,
}

impl Kinds {
    pub fn from_feed(kinds: &BTreeMap<String, KindDoc>) -> Self {
        Self {
            parents: kinds
                .iter()
                .map(|(name, kind)| (name.clone(), kind.parent.clone()))
                .collect(),
        }
    }

    pub fn contains(&self, kind: &str) -> bool {
        self.parents.contains_key(kind)
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
}
