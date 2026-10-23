//! The config: `plat5.yml`'s keys, with `services` as a map of name to service
//! (docs/concepts/config.md). Parsed only after the schema passed, so these types
//! hold shape the schema already checked. Contract for every field:
//! docs/reference/config/.

use std::collections::{BTreeMap, BTreeSet};

use serde::{Deserialize, Serialize};
use serde_json::Value;

pub const DEFAULT_DELETION_GRACE: &str = "30d";
pub const PUBLIC_SCOPE: &str = "public";

#[derive(Clone, Debug, Deserialize, PartialEq)]
pub struct Config {
    pub version: u32,
    #[serde(default = "default_deletion_grace")]
    pub deletion_grace: String,
    #[serde(default)]
    pub reserved_slugs: Vec<String>,
    pub kinds: BTreeMap<String, Kind>,
    pub roles: BTreeMap<String, Role>,
    pub default_role: String,
    pub creator_role: String,
    #[serde(default)]
    pub cors: Cors,
    #[serde(default)]
    pub services: BTreeMap<String, Service>,
}

fn default_deletion_grace() -> String {
    DEFAULT_DELETION_GRACE.to_string()
}

#[derive(Clone, Debug, Default, Deserialize, Serialize, PartialEq)]
pub struct Kind {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub parent: Option<Parents>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub label: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub sign_in: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub name_claim: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub segment: Option<String>,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
#[serde(untagged)]
pub enum Parents {
    One(String),
    Many(Vec<String>),
}

impl Kind {
    /// The kinds a node of this kind sits under. Empty for a root.
    pub fn parents(&self) -> Vec<String> {
        match &self.parent {
            None => Vec::new(),
            Some(Parents::One(p)) => vec![p.clone()],
            Some(Parents::Many(ps)) => ps.clone(),
        }
    }

    pub fn is_root(&self) -> bool {
        self.parents().is_empty()
    }

    pub fn is_sign_in(&self) -> bool {
        self.sign_in.is_some()
    }

    /// The path segment for generated routes: `segment`, or the name with `_`
    /// as `-`, plus `s`, so a derived segment has the slug shape too.
    pub fn segment(&self, name: &str) -> String {
        self.segment
            .clone()
            .unwrap_or_else(|| format!("{}s", name.replace('_', "-")))
    }
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
pub struct Role {
    pub labels: Vec<String>,
    pub bindable_at: Vec<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub max_duration: Option<String>,
}

#[derive(Clone, Debug, Default, Deserialize, Serialize, PartialEq)]
pub struct Cors {
    #[serde(default)]
    pub allowed_origins: Vec<String>,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
pub struct Timeouts {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub connect_ms: Option<u64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub read_ms: Option<u64>,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
pub struct Policy {
    pub requests: u64,
    pub window_seconds: u64,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub count_at: Option<String>,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub shared: bool,
}

#[derive(Clone, Debug, Deserialize, PartialEq)]
pub struct Service {
    pub url: String,
    #[serde(default)]
    pub rate_limits: BTreeMap<String, Policy>,
    #[serde(default)]
    pub timeouts: Option<Timeouts>,
    #[serde(default)]
    pub route_prefix: Option<String>,
    pub routes: Vec<Route>,
}

#[derive(Clone, Debug, Deserialize, PartialEq)]
pub struct Route {
    pub path: String,
    pub scope: String,
    #[serde(default)]
    pub upstream: Option<String>,
    pub methods: Methods,
    #[serde(default)]
    pub required_labels: Option<Vec<String>>,
    #[serde(default)]
    pub rate_limit: Option<RateLimit>,
    #[serde(default)]
    pub audit: Option<bool>,
    #[serde(default)]
    pub resolve_deleted: Option<bool>,
}

#[derive(Clone, Debug, Deserialize, PartialEq)]
#[serde(untagged)]
pub enum Methods {
    List(Vec<String>),
    /// Verb → its config. A verb written with no body (`GET:`) is `None`.
    Map(BTreeMap<String, Option<MethodConfig>>),
}

#[derive(Clone, Debug, Default, Deserialize, PartialEq)]
pub struct MethodConfig {
    #[serde(default)]
    pub required_labels: Option<Vec<String>>,
    #[serde(default)]
    pub rate_limit: Option<RateLimit>,
    #[serde(default)]
    pub audit: Option<bool>,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
#[serde(untagged)]
pub enum RateLimit {
    /// Only `false`; the schema refuses `true`.
    Off(bool),
    Name(String),
    Names(Vec<String>),
    Inline {
        requests: u64,
        window_seconds: u64,
    },
}

impl RateLimit {
    /// The named policies this rate limit uses.
    pub fn names(&self) -> Vec<&str> {
        match self {
            RateLimit::Name(n) => vec![n.as_str()],
            RateLimit::Names(ns) => ns.iter().map(String::as_str).collect(),
            _ => Vec::new(),
        }
    }
}

/// One method of a route, after `route_prefix` is joined and the methods map is
/// expanded: what one stored row says about one verb.
#[derive(Clone, Debug, PartialEq)]
pub struct MethodSpec<'a> {
    pub method: &'a str,
    pub required_labels: Option<&'a Vec<String>>,
    pub rate_limit: Option<&'a RateLimit>,
    pub audit: Option<bool>,
}

impl Route {
    /// Each method with the config that applies to it, in either form.
    pub fn method_specs(&self) -> Vec<MethodSpec<'_>> {
        match &self.methods {
            Methods::List(verbs) => verbs
                .iter()
                .map(|m| MethodSpec {
                    method: m,
                    required_labels: self.required_labels.as_ref(),
                    rate_limit: self.rate_limit.as_ref(),
                    audit: self.audit,
                })
                .collect(),
            Methods::Map(map) => map
                .iter()
                .map(|(m, cfg)| MethodSpec {
                    method: m,
                    required_labels: cfg.as_ref().and_then(|c| c.required_labels.as_ref()),
                    rate_limit: cfg.as_ref().and_then(|c| c.rate_limit.as_ref()),
                    audit: cfg.as_ref().and_then(|c| c.audit),
                })
                .collect(),
        }
    }
}

/// `route_prefix` joined to a route path. Never applied to `upstream`.
pub fn join_prefix(prefix: Option<&str>, path: &str) -> String {
    match prefix {
        None => path.to_string(),
        Some(prefix) => {
            let base = prefix.trim_end_matches('/');
            if path == "/" {
                if base.is_empty() {
                    "/".to_string()
                } else {
                    base.to_string()
                }
            } else {
                format!("{base}{path}")
            }
        }
    }
}

/// A route in the stored form the feeds serve: full path, one list of methods.
#[derive(Clone, Debug, Serialize, PartialEq)]
pub struct StoredRoute {
    pub path: String,
    pub scope: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub upstream: Option<String>,
    pub methods: Vec<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub required_labels: Option<Vec<String>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub rate_limit: Option<RateLimit>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub audit: Option<bool>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub resolve_deleted: Option<bool>,
}

#[derive(Clone, Debug, Serialize, PartialEq)]
pub struct StoredService {
    pub url: String,
    #[serde(skip_serializing_if = "BTreeMap::is_empty")]
    pub rate_limits: BTreeMap<String, Policy>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub timeouts: Option<Timeouts>,
    pub routes: Vec<StoredRoute>,
}

impl Service {
    /// The stored form: `route_prefix` joined, a methods map expanded into one
    /// route per verb (docs/reference/config/routes.md#methods).
    pub fn stored(&self) -> StoredService {
        let mut routes = Vec::new();
        for route in &self.routes {
            let path = join_prefix(self.route_prefix.as_deref(), &route.path);
            let base = StoredRoute {
                path,
                scope: route.scope.clone(),
                upstream: route.upstream.clone(),
                methods: Vec::new(),
                required_labels: None,
                rate_limit: None,
                audit: None,
                resolve_deleted: route.resolve_deleted,
            };
            match &route.methods {
                Methods::List(verbs) => routes.push(StoredRoute {
                    methods: verbs.clone(),
                    required_labels: route.required_labels.clone(),
                    rate_limit: route.rate_limit.clone(),
                    audit: route.audit,
                    ..base
                }),
                Methods::Map(_) => {
                    for spec in route.method_specs() {
                        routes.push(StoredRoute {
                            methods: vec![spec.method.to_string()],
                            required_labels: spec.required_labels.cloned(),
                            rate_limit: spec.rate_limit.cloned(),
                            audit: spec.audit,
                            ..base.clone()
                        });
                    }
                }
            }
        }
        StoredService {
            url: self.url.clone(),
            rate_limits: self.rate_limits.clone(),
            timeouts: self.timeouts.clone(),
            routes,
        }
    }

    /// Every method of every route, keyed by `(path, method)`, each with what the
    /// gateway does for it. What the plan compares.
    pub fn method_rows(&self) -> BTreeMap<(String, String), Value> {
        let mut out = BTreeMap::new();
        for route in self.stored().routes {
            for method in &route.methods {
                let row = StoredRoute {
                    methods: vec![method.clone()],
                    ..route.clone()
                };
                out.insert(
                    (route.path.clone(), method.clone()),
                    serde_json::to_value(row).expect("stored route serializes"),
                );
            }
        }
        out
    }
}

impl Config {
    /// A config holding only kinds: what `POST /catalog` checks and generates from.
    pub fn from_kinds(kinds: BTreeMap<String, Kind>) -> Self {
        Self {
            version: 2,
            deletion_grace: default_deletion_grace(),
            reserved_slugs: Vec::new(),
            kinds,
            roles: BTreeMap::new(),
            default_role: String::new(),
            creator_role: String::new(),
            cors: Cors::default(),
            services: BTreeMap::new(),
        }
    }

    /// The one kind marked `sign_in`. Rule 1 makes it exist once validated.
    pub fn sign_in_kind(&self) -> Option<&str> {
        self.kinds
            .iter()
            .find(|(_, k)| k.is_sign_in())
            .map(|(n, _)| n.as_str())
    }

    /// Kinds that can be created: every kind but the sign-in kind.
    pub fn creatable_kinds(&self) -> impl Iterator<Item = &str> {
        self.kinds
            .iter()
            .filter(|(_, k)| !k.is_sign_in())
            .map(|(n, _)| n.as_str())
    }

    /// Every chain of kinds from a root down to `kind`, root first. A kind with
    /// several parents has several chains. Assumes no cycles (rule 2).
    pub fn chains(&self, kind: &str) -> Vec<Vec<String>> {
        let Some(k) = self.kinds.get(kind) else {
            return Vec::new();
        };
        let parents = k.parents();
        if parents.is_empty() {
            return vec![vec![kind.to_string()]];
        }
        let mut out = Vec::new();
        for parent in parents {
            for mut chain in self.chains(&parent) {
                chain.push(kind.to_string());
                out.push(chain);
            }
        }
        out
    }

    pub fn stored_services(&self) -> BTreeMap<String, StoredService> {
        self.services
            .iter()
            .map(|(n, s)| (n.clone(), s.stored()))
            .collect()
    }

    pub fn reserved_slug_set(&self) -> BTreeSet<&str> {
        self.reserved_slugs.iter().map(String::as_str).collect()
    }
}

/// The keys of the flat config that aren't the config: the apply's preconditions.
pub const APPLY_ONLY_KEYS: &[&str] = &["expect", "confirm"];
