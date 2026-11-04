//! The config: `plat5.yml`'s keys, with `services` as a map of name to service
//! (docs/concepts/config.md). Parsed only after the schema passed, so these types
//! hold shape the schema already checked. Contract for every field:
//! docs/reference/config/.

use std::collections::BTreeMap;

use serde::{Deserialize, Serialize};

pub const DEFAULT_DELETION_GRACE: &str = "30d";
/// `users.name.claims` when the config names none.
pub const DEFAULT_NAME_CLAIMS: [&str; 2] = ["name", "preferred_username"];
/// `route_checks.path` when the config names none.
pub const DEFAULT_ROUTE_CHECKS_PATH: &str = "/access/routes";
pub const DEFAULT_CONNECT_TIMEOUT: &str = "10s";
pub const DEFAULT_READ_TIMEOUT: &str = "30s";
pub const DEFAULT_MAX_BODY_BYTES: u64 = 10_485_760;

pub const USER_SCOPE: &str = "user";
pub const PUBLIC_SCOPE: &str = "public";
/// Every struct here refuses unknown fields: a config the running version
/// can't read fails loudly, never read with a field dropped
/// (docs/concepts/config.md#the-config).
#[derive(Clone, Debug, Deserialize, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct Config {
    #[serde(default = "default_deletion_grace")]
    pub deletion_grace: String,
    #[serde(default)]
    pub reserved_slugs: Vec<String>,
    #[serde(default)]
    pub users: Users,
    #[serde(default)]
    pub route_checks: RouteChecks,
    pub defaults: Defaults,
    #[serde(deserialize_with = "kinds_filled")]
    pub kinds: BTreeMap<String, Kind>,
    pub roles: BTreeMap<String, Role>,
    pub creator_role: String,
    pub rate_limits: BTreeMap<String, Policy>,
    #[serde(default)]
    pub cors: Cors,
    #[serde(default)]
    pub services: BTreeMap<String, Service>,
}

fn default_deletion_grace() -> String {
    DEFAULT_DELETION_GRACE.to_string()
}

/// `users:`: how Plat5 reads a person from the IdP's token.
#[derive(Clone, Debug, Default, Deserialize, Serialize, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct Users {
    #[serde(default)]
    pub name: UserName,
}

/// `users.name`: the claims a sign-in reads a person's name from, and when it
/// sets it (docs/reference/idp.md#a-persons-name).
#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct UserName {
    #[serde(default = "default_name_claims")]
    pub claims: Vec<String>,
    #[serde(default)]
    pub sync: NameSync,
}

impl Default for UserName {
    fn default() -> Self {
        Self {
            claims: default_name_claims(),
            sync: NameSync::default(),
        }
    }
}

fn default_name_claims() -> Vec<String> {
    DEFAULT_NAME_CLAIMS.iter().map(|c| c.to_string()).collect()
}

/// `route_checks:`: where the gateway answers route checks
/// (docs/reference/gateway.md#route-checks).
#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct RouteChecks {
    #[serde(default = "default_route_checks_path")]
    pub path: String,
}

impl Default for RouteChecks {
    fn default() -> Self {
        Self {
            path: default_route_checks_path(),
        }
    }
}

fn default_route_checks_path() -> String {
    DEFAULT_ROUTE_CHECKS_PATH.to_string()
}

/// `users.name.sync`.
#[derive(Clone, Copy, Debug, Default, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum NameSync {
    /// A sign-in sets the name only while it's empty.
    #[default]
    FirstSignIn,
    /// Every sign-in with a non-empty claim sets it; the person can't.
    EachSignIn,
}

#[derive(Clone, Debug, Deserialize, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct Defaults {
    /// The policies of every route that names none, and what the gateway
    /// counts route checks and a service account on a user route against.
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
/// and route at expansion.
#[derive(Clone, Debug, PartialEq)]
pub struct FilledDefaults {
    pub rate_limit: Vec<String>,
    pub timeouts: FilledTimeouts,
    pub max_body_bytes: u64,
}

impl Defaults {
    /// `defaults:` with Plat5's own defaults under it.
    pub fn filled(&self) -> FilledDefaults {
        FilledDefaults {
            rate_limit: self.rate_limit.clone(),
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
    /// `<kind name>:read` when unset: filled in as the config is parsed,
    /// so readers always get one.
    #[serde(default)]
    pub list_permission: String,
    /// Whether creating a node of this kind binds its creator with `creator_role`.
    /// Sent only when off, so a reader takes a missing value as on.
    #[serde(default = "yes", skip_serializing_if = "is_yes")]
    pub bind_creator: bool,
}

/// The kinds, each `list_permission` filled in where the config leaves it out.
fn kinds_filled<'de, D: serde::Deserializer<'de>>(
    d: D,
) -> Result<BTreeMap<String, Kind>, D::Error> {
    let mut kinds = BTreeMap::<String, Kind>::deserialize(d)?;
    for (name, kind) in kinds.iter_mut() {
        if kind.list_permission.is_empty() {
            kind.list_permission = format!("{name}:read");
        }
    }
    Ok(kinds)
}

fn yes() -> bool {
    true
}

fn is_yes(b: &bool) -> bool {
    *b
}

impl Kind {
    pub fn is_root(&self) -> bool {
        self.parent.is_none()
    }

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

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct Policy {
    pub requests: u64,
    pub window_seconds: u64,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub count_at: Option<String>,
}

#[derive(Clone, Debug, Default, Deserialize, Serialize, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct Cors {
    #[serde(default)]
    pub allowed_origins: Vec<String>,
    #[serde(default)]
    pub allowed_headers: Vec<String>,
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
    #[serde(default)]
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
    pub required_permissions: Option<Required>,
    #[serde(default)]
    pub rate_limit: Option<RateLimit>,
    #[serde(default)]
    pub audit: Option<bool>,
    #[serde(default)]
    pub allow_deleted: Option<bool>,
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
    pub required_permissions: Option<Required>,
    #[serde(default)]
    pub rate_limit: Option<RateLimit>,
    #[serde(default)]
    pub audit: Option<bool>,
}

/// `required_permissions`: a list, any one of which admits, or `any_member`.
#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
#[serde(untagged)]
pub enum Required {
    Permissions(Vec<String>),
    Keyword(Keyword),
}

#[derive(Clone, Copy, Debug, Deserialize, Serialize, PartialEq)]
pub enum Keyword {
    #[serde(rename = "any_member")]
    AnyMember,
}

impl Required {
    pub fn permissions(&self) -> &[String] {
        match self {
            Required::Permissions(p) => p,
            Required::Keyword(_) => &[],
        }
    }
}

/// `rate_limit`: policy names, or `false` for none (the schema refuses `true`).
#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
#[serde(untagged)]
pub enum RateLimit {
    Off(bool),
    Policies(Vec<String>),
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

/// The key of the flat config that isn't the config: the apply's precondition.
pub const APPLY_ONLY_KEYS: &[&str] = &["expect"];
