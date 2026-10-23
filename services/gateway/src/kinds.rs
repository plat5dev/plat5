//! The deployment's kinds, as the gateway uses them: which kind is the sign-in
//! kind, its name claim, and each kind's parents for checking a route's chain
//! (docs/reference/config/kinds.md).

use std::collections::{BTreeMap, HashMap};

use serde::Deserialize;

const DEFAULT_NAME_CLAIM: &str = "name";

/// One kind as the feed sends it. Fields the gateway doesn't use are ignored.
#[derive(Clone, Debug, Deserialize)]
pub struct KindDoc {
    #[serde(default)]
    pub parent: Option<Parents>,
    #[serde(default)]
    pub sign_in: Option<String>,
    #[serde(default)]
    pub name_claim: Option<String>,
}

#[derive(Clone, Debug, Deserialize)]
#[serde(untagged)]
pub enum Parents {
    One(String),
    Many(Vec<String>),
}

#[derive(Clone, Debug)]
pub struct Kinds {
    sign_in: String,
    name_claim: String,
    parents: HashMap<String, Vec<String>>,
}

impl Kinds {
    pub fn from_feed(kinds: &BTreeMap<String, KindDoc>) -> Result<Self, String> {
        let mut sign_in = None;
        let mut parents = HashMap::new();
        for (name, kind) in kinds {
            if kind.sign_in.is_some() {
                if sign_in.is_some() {
                    return Err("more than one kind is marked sign_in".to_string());
                }
                sign_in = Some((name.clone(), kind.name_claim.clone()));
            }
            let list = match &kind.parent {
                None => Vec::new(),
                Some(Parents::One(p)) => vec![p.clone()],
                Some(Parents::Many(ps)) => ps.clone(),
            };
            parents.insert(name.clone(), list);
        }
        let Some((sign_in, name_claim)) = sign_in else {
            return Err("no kind is marked sign_in".to_string());
        };
        for (name, ps) in &parents {
            if let Some(p) = ps.iter().find(|p| !parents.contains_key(*p)) {
                return Err(format!("kind {name}'s parent {p} is not a kind"));
            }
        }
        Ok(Self {
            sign_in,
            name_claim: name_claim.unwrap_or_else(|| DEFAULT_NAME_CLAIM.to_string()),
            parents,
        })
    }

    pub fn contains(&self, kind: &str) -> bool {
        self.parents.contains_key(kind)
    }

    /// The sign-in kind's name, e.g. `directory`.
    pub fn sign_in(&self) -> &str {
        &self.sign_in
    }

    /// The IdP claim holding a person's name.
    pub fn name_claim(&self) -> &str {
        &self.name_claim
    }

    /// Whether `chain` (root first) is a path down the tree: a root that isn't
    /// the sign-in kind, then each kind under the one before it.
    pub fn is_chain(&self, chain: &[String]) -> bool {
        let Some(first) = chain.first() else {
            return false;
        };
        if first == &self.sign_in || !self.parents.get(first).is_some_and(|p| p.is_empty()) {
            return false;
        }
        chain.windows(2).all(|pair| {
            self.parents
                .get(&pair[1])
                .is_some_and(|ps| ps.contains(&pair[0]))
        })
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

    fn kinds() -> Kinds {
        let docs: BTreeMap<String, KindDoc> = serde_json::from_value(json!({
            "directory": { "sign_in": "idp", "name_claim": "full_name" },
            "organization": {},
            "folder": { "parent": "organization" },
            "project": { "parent": ["organization", "folder"] }
        }))
        .unwrap();
        Kinds::from_feed(&docs).unwrap()
    }

    #[test]
    fn headers_title_case_each_part() {
        assert_eq!(id_header("project"), "X-Plat5-Project-Id");
        assert_eq!(id_header("cost_center"), "X-Plat5-Cost-Center-Id");
    }

    #[test]
    fn chains_run_from_a_root_down() {
        let k = kinds();
        assert_eq!(k.sign_in(), "directory");
        assert_eq!(k.name_claim(), "full_name");
        let chain = |c: &[&str]| k.is_chain(&c.iter().map(|s| s.to_string()).collect::<Vec<_>>());
        assert!(chain(&["organization"]));
        assert!(chain(&["organization", "project"]));
        assert!(chain(&["organization", "folder", "project"]));
        assert!(!chain(&["project"]));
        assert!(!chain(&["folder", "organization"]));
        assert!(!chain(&["directory"]));
        assert!(!chain(&[]));
    }

    #[test]
    fn exactly_one_sign_in_kind() {
        let none: BTreeMap<String, KindDoc> =
            serde_json::from_value(json!({ "organization": {} })).unwrap();
        assert!(Kinds::from_feed(&none).is_err());
        let two: BTreeMap<String, KindDoc> =
            serde_json::from_value(json!({ "a": { "sign_in": "idp" }, "b": { "sign_in": "idp" } }))
                .unwrap();
        assert!(Kinds::from_feed(&two).is_err());
    }
}
