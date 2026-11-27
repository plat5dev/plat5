//! The config: `plat5.yml`'s keys, with `services` as a map of name to service
//! (docs/concepts/config.md). Parsed only after the schema passed, so these types
//! hold shape the schema already checked. Contract for every field:
//! docs/reference/config/.

use std::collections::BTreeMap;

use serde::{Deserialize, Serialize};

pub const DEFAULT_DELETION_GRACE: &str = "30d";
pub const ROUTE_CHECKS_PATH: &str = "/access/routes";
pub const DEFAULT_CONNECT_TIMEOUT: &str = "10s";
pub const DEFAULT_READ_TIMEOUT: &str = "30s";
pub const DEFAULT_MAX_BODY_BYTES: u64 = 10_485_760;
pub const DEFAULT_AUTHORIZER_TIMEOUT: &str = "2s";
pub const DEFAULT_AUTHORIZER_CACHE_TTL: &str = "0s";

pub const USER_SCOPE: &str = "user";
pub const PUBLIC_SCOPE: &str = "public";

/// Every struct here refuses unknown fields: a config the running version
/// can't read fails loudly, never read with a field dropped
/// (docs/concepts/config.md#the-config-shape).
#[derive(Clone, Debug, Deserialize, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct Config {
    #[serde(default = "default_deletion_grace")]
    pub deletion_grace: String,
    #[serde(default)]
    pub reserved_slugs: Vec<String>,
    #[serde(default)]
    pub policy_claims: Vec<String>,
    #[serde(default)]
    pub defaults: Defaults,
    pub kinds: BTreeMap<String, Kind>,
    #[serde(default)]
    pub roles: BTreeMap<String, Role>,
    #[serde(default)]
    pub creator_role: Option<String>,
    #[serde(default)]
    pub custom_roles: bool,
    #[serde(default)]
    pub permissions: BTreeMap<String, PermissionMeta>,
    #[serde(default)]
    pub authorizer: Option<Authorizer>,
    #[serde(default)]
    pub rate_limits: BTreeMap<String, Policy>,
    #[serde(default)]
    pub services: BTreeMap<String, Service>,
}

fn default_deletion_grace() -> String {
    DEFAULT_DELETION_GRACE.to_string()
}

#[derive(Clone, Debug, Default, Deserialize, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct Defaults {
    /// The policies of every route that names none, and what the gateway
    /// counts `/access/routes` against.
    #[serde(default)]
    pub rate_limit: Vec<String>,
    #[serde(default)]
    pub timeouts: Timeouts,
    #[serde(default)]
    pub max_body_bytes: Option<u64>,
}

#[derive(Clone, Debug, Default, Deserialize, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct Timeouts {
    #[serde(default)]
    pub connect: Option<String>,
    #[serde(default)]
    pub read: Option<String>,
}

/// Timeouts with every one filled in: what the gateway is served.
#[derive(Clone, Debug, Serialize, PartialEq)]
pub struct FilledTimeouts {
    pub connect: String,
    pub read: String,
}

/// `defaults:` with Plat5's own defaults under it, filled into each service
/// at expansion.
#[derive(Clone, Debug, PartialEq)]
pub struct FilledDefaults {
    pub timeouts: FilledTimeouts,
    pub max_body_bytes: u64,
}

impl Defaults {
    /// `defaults:` with Plat5's own defaults under it.
    pub fn filled(&self) -> FilledDefaults {
        FilledDefaults {
            timeouts: FilledTimeouts {
                connect: self
                    .timeouts
                    .connect
                    .clone()
                    .unwrap_or_else(|| DEFAULT_CONNECT_TIMEOUT.to_string()),
                read: self
                    .timeouts
                    .read
                    .clone()
                    .unwrap_or_else(|| DEFAULT_READ_TIMEOUT.to_string()),
            },
            max_body_bytes: self.max_body_bytes.unwrap_or(DEFAULT_MAX_BODY_BYTES),
        }
    }
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct Kind {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub parent: Option<String>,
    pub path: String,
    /// The permission that shows a node of this kind in a list.
    pub list_permission: String,
    /// Whether creating a node of this kind binds its creator with `creator_role`.
    /// Sent only when off, so a reader takes a missing value as on.
    #[serde(default = "yes", skip_serializing_if = "is_yes")]
    pub bind_creator: bool,
    /// Optional hostname template on a root kind: `{<kind>}.<domain>`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub host: Option<String>,
}

fn yes() -> bool {
    true
}

fn is_yes(b: &bool) -> bool {
    *b
}

impl Kind {
    /// The literal of the kind's path piece: `/projects` for `/projects/{project}`.
    pub fn collection(&self) -> &str {
        match self.path[1..].find('/') {
            Some(i) => &self.path[..i + 1],
            None => &self.path,
        }
    }

    /// The param of the kind's path piece: `project` for `/projects/{project}`.
    pub fn param(&self) -> &str {
        self.path
            .rsplit_once('{')
            .map(|(_, p)| p.trim_end_matches('}'))
            .unwrap_or_default()
    }

    /// For `host: "{organization}.api.example.com"`, returns `("organization", ".api.example.com")`.
    pub fn host_parts(&self) -> Option<(&str, &str)> {
        let host = self.host.as_deref()?;
        let rest = host.strip_prefix('{')?;
        let (param, suffix) = rest.split_once('}')?;
        Some((param, suffix))
    }
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct Role {
    #[serde(default)]
    pub permissions: Vec<String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub includes: Vec<String>,
    pub bindable_at: Vec<String>,
}

#[derive(Clone, Debug, Deserialize, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct PermissionMeta {
    #[serde(default)]
    pub description: String,
    #[serde(default)]
    pub bindable_at: Vec<String>,
    #[serde(default = "yes")]
    pub custom_role_assignable: bool,
}

fn default_authorizer_timeout() -> String {
    DEFAULT_AUTHORIZER_TIMEOUT.to_string()
}

fn default_authorizer_cache_ttl() -> String {
    DEFAULT_AUTHORIZER_CACHE_TTL.to_string()
}

/// Top-level external AuthZEN 1.0 PDP definition (`authorizer`).
#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct Authorizer {
    pub url: String,
    #[serde(default = "default_authorizer_timeout")]
    pub timeout: String,
    #[serde(default = "default_authorizer_cache_ttl")]
    pub cache_ttl: String,
    #[serde(default = "default_authorizer_cache_ttl")]
    pub negative_cache_ttl: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub token_env: Option<String>,
}

/// Optional AuthZEN resource override on a route or verb (`{ type, id }`).
#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
pub struct Resource {
    #[serde(rename = "type")]
    pub r#type: String,
    pub id: String,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct TierLimit {
    pub requests: u64,
    pub window: String,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct Policy {
    pub requests: u64,
    pub window: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub count_at: Option<String>,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    pub tiers: BTreeMap<String, TierLimit>,
}

/// One service: identity and audit are services like any other.
#[derive(Clone, Debug, Deserialize, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct Service {
    pub url: String,
    #[serde(default)]
    pub timeouts: Timeouts,
    #[serde(default)]
    pub max_body_bytes: Option<u64>,
    pub scopes: BTreeMap<String, Scope>,
}

#[derive(Clone, Debug, Deserialize, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct Scope {
    #[serde(default)]
    pub upstream_prefix: Option<String>,
    pub routes: Vec<Route>,
}

#[derive(Clone, Debug, Deserialize, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct Route {
    pub path: String,
    #[serde(default)]
    pub upstream: Option<String>,
    pub methods: Methods,
    #[serde(default)]
    pub permissions: Option<Required>,
    #[serde(default)]
    pub rate_limit: Option<RateLimit>,
    #[serde(default)]
    pub audit: Option<bool>,
    #[serde(default)]
    pub allow_deleted: Option<bool>,
    #[serde(default)]
    pub resource: Option<Resource>,
}

#[derive(Clone, Debug, Deserialize, PartialEq)]
#[serde(untagged)]
pub enum Methods {
    List(Vec<String>),
    /// Verb → its overrides. A verb written with no body (`GET:`) is `None`.
    Map(BTreeMap<String, Option<MethodConfig>>),
}

#[derive(Clone, Debug, Default, Deserialize, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct MethodConfig {
    #[serde(default)]
    pub permissions: Option<Required>,
    #[serde(default)]
    pub rate_limit: Option<RateLimit>,
    #[serde(default)]
    pub audit: Option<bool>,
    #[serde(default)]
    pub resource: Option<Resource>,
}

/// `permissions`: a list (or single-string shorthand), any one of which admits,
/// or `any_member`. Always serializes canonically as an array or `"any_member"`.
#[derive(Clone, Debug, Serialize, PartialEq)]
#[serde(untagged)]
pub enum Required {
    Permissions(Vec<String>),
    Keyword(Keyword),
}

#[derive(Deserialize)]
#[serde(untagged)]
enum RequiredRaw {
    Single(String),
    List(Vec<String>),
}

impl<'de> serde::Deserialize<'de> for Required {
    fn deserialize<D: serde::Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        match RequiredRaw::deserialize(d)? {
            RequiredRaw::Single(s) if s == "any_member" => {
                Ok(Required::Keyword(Keyword::AnyMember))
            }
            RequiredRaw::Single(s) => Ok(Required::Permissions(vec![s])),
            RequiredRaw::List(list) => Ok(Required::Permissions(list)),
        }
    }
}

#[derive(Clone, Copy, Debug, Deserialize, Serialize, PartialEq)]
pub enum Keyword {
    #[serde(rename = "any_member")]
    AnyMember,
}

/// `rate_limit`: policy names, or `false` for none.
#[derive(Clone, Debug, Serialize, PartialEq)]
#[serde(untagged)]
pub enum RateLimit {
    Off(bool),
    Policies(Vec<String>),
}

#[derive(Deserialize)]
#[serde(untagged)]
enum RateLimitRaw {
    Flag(bool),
    Policies(Vec<String>),
}

impl<'de> serde::Deserialize<'de> for RateLimit {
    fn deserialize<D: serde::Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        match RateLimitRaw::deserialize(d)? {
            RateLimitRaw::Flag(false) => Ok(RateLimit::Off(false)),
            RateLimitRaw::Flag(true) => Err(serde::de::Error::custom(
                "rate_limit must be a list of policies or false",
            )),
            RateLimitRaw::Policies(p) => Ok(RateLimit::Policies(p)),
        }
    }
}

/// The verbs a route can have, in the order expanded routes list them.
pub const VERBS: &[&str] = &["GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"];

/// Audited unless a route says otherwise: the writes.
pub fn audited_by_default(method: &str) -> bool {
    matches!(method, "POST" | "PUT" | "PATCH" | "DELETE")
}

impl Config {
    /// The chain of kinds from the root down to `kind`, root first. Assumes the
    /// parents hold (rule 1).
    pub fn chain(&self, kind: &str) -> Vec<String> {
        let mut chain = vec![kind.to_string()];
        let mut at = kind;
        while let Some(parent) = self.kinds.get(at).and_then(|k| k.parent.as_deref()) {
            if chain.iter().any(|k| k == parent) {
                break;
            }
            chain.push(parent.to_string());
            at = parent;
        }
        chain.reverse();
        chain
    }

    /// The public path of a node of `kind`: each kind's piece, root first.
    pub fn node_path(&self, kind: &str) -> String {
        self.chain(kind)
            .iter()
            .map(|k| self.kinds[k].path.as_str())
            .collect()
    }

    /// The kinds whose parent is `kind`.
    pub fn children<'a>(&'a self, kind: &'a str) -> impl Iterator<Item = (&'a String, &'a Kind)> {
        self.kinds
            .iter()
            .filter(move |(_, k)| k.parent.as_deref() == Some(kind))
    }
}
