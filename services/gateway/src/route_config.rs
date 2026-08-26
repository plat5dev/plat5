use serde::de::{self, Deserializer, IgnoredAny};
use serde::{Deserialize, Serialize};
use std::collections::{BTreeMap, HashMap, HashSet};

/// etcd key prefix for gateway route registry entries.
pub const ROUTES_PREFIX: &str = "edge/gateway/routes/";

pub const MAX_SCOPE_COUNT: usize = 32;
pub const MAX_SCOPE_LEN: usize = 64;

const HTTP_METHODS: &[&str] = &["GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"];

#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct Config {
    pub services: HashMap<String, ServiceConfig>,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct ServiceConfig {
    pub url: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub public: Option<ScopeConfig>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub user: Option<ScopeConfig>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub organization: Option<ScopeConfig>,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct ScopeConfig {
    /// Optional path prefix. Expanded into each route `path` at write time.
    /// Etcd stores full paths; gateway never requires this field.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub route_prefix: Option<String>,
    /// Required on `organization` scope — path param name for org id.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub organization_param: Option<String>,
    pub routes: Vec<RouteConfig>,
}

/// Apply-time nested method body. Empty (`GET:` / `GET: {}`) means that verb, no extra constraints.
#[derive(Clone, Debug, Default, Deserialize, Serialize, PartialEq)]
pub struct MethodConfig {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub required_scopes: Option<Vec<String>>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub rate_limit: Option<RouteRateLimit>,
}

#[derive(Clone, Debug, Default, PartialEq)]
pub(crate) enum MethodsForm {
    #[default]
    List,
    Nested(Vec<(String, MethodConfig)>),
    Mixed,
}

#[derive(Clone, Debug, Serialize)]
pub struct RouteConfig {
    pub path: String,
    pub methods: Vec<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub transform: Option<TransformConfig>,
    /// Optional API-key scope labels this route requires.
    /// Omitted = any admitted principal. JWTs and unrestricted keys skip the check.
    /// Route-level value applies only to the flat methods list form.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub required_scopes: Option<Vec<String>>,
    /// Omitted = inherit gateway fallback. `false` = unlimited. Object = override.
    /// Route-level value applies only to the flat methods list form.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub rate_limit: Option<RouteRateLimit>,
    /// Nested methods map, if the YAML/JSON used the per-verb object form.
    /// Cleared when expanded at apply. Never written to etcd.
    #[serde(skip)]
    pub(crate) methods_form: MethodsForm,
}

#[derive(Deserialize)]
#[serde(untagged)]
enum RawMethods {
    List(Vec<String>),
    Nested(BTreeMap<String, Option<MethodConfig>>),
    #[allow(dead_code)] // serde match only; payload unread
    MixedSeq(Vec<IgnoredAny>),
}

#[derive(Deserialize)]
struct RawRouteConfig {
    path: String,
    methods: RawMethods,
    #[serde(default)]
    transform: Option<TransformConfig>,
    #[serde(default)]
    required_scopes: Option<Vec<String>>,
    #[serde(default)]
    rate_limit: Option<RouteRateLimit>,
}

impl<'de> Deserialize<'de> for RouteConfig {
    fn deserialize<D: Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        let raw = RawRouteConfig::deserialize(deserializer)?;
        match raw.methods {
            RawMethods::List(methods) => Ok(RouteConfig {
                path: raw.path,
                methods,
                transform: raw.transform,
                required_scopes: raw.required_scopes,
                rate_limit: raw.rate_limit,
                methods_form: MethodsForm::List,
            }),
            RawMethods::Nested(map) => {
                if map.is_empty() {
                    return Ok(RouteConfig {
                        path: raw.path,
                        methods: Vec::new(),
                        transform: raw.transform,
                        required_scopes: raw.required_scopes,
                        rate_limit: raw.rate_limit,
                        methods_form: MethodsForm::Nested(Vec::new()),
                    });
                }
                let entries: Vec<(String, MethodConfig)> = map
                    .into_iter()
                    .map(|(k, v)| (k, v.unwrap_or_default()))
                    .collect();
                let methods = entries.iter().map(|(m, _)| m.clone()).collect();
                Ok(RouteConfig {
                    path: raw.path,
                    methods,
                    transform: raw.transform,
                    required_scopes: raw.required_scopes,
                    rate_limit: raw.rate_limit,
                    methods_form: MethodsForm::Nested(entries),
                })
            }
            RawMethods::MixedSeq(_) => Ok(RouteConfig {
                path: raw.path,
                methods: Vec::new(),
                transform: raw.transform,
                required_scopes: raw.required_scopes,
                rate_limit: raw.rate_limit,
                methods_form: MethodsForm::Mixed,
            }),
        }
    }
}

impl Default for RouteConfig {
    fn default() -> Self {
        Self {
            path: String::new(),
            methods: Vec::new(),
            transform: None,
            required_scopes: None,
            rate_limit: None,
            methods_form: MethodsForm::List,
        }
    }
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct TransformConfig {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub path: Option<String>,
}

/// Per-route rate limit. `false` opts out (unlimited). Object overrides the gateway fallback.
#[derive(Clone, Debug, PartialEq)]
pub enum RouteRateLimit {
    Unlimited,
    Limit(RateLimitConfig),
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
pub struct RateLimitConfig {
    pub requests: u64,
    pub window_seconds: u64,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub by: Option<String>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum RateLimitBy {
    Ip,
    User,
    Member,
}

impl RateLimitBy {
    pub fn parse(raw: &str) -> Option<Self> {
        match raw.trim() {
            "ip" => Some(Self::Ip),
            "user" => Some(Self::User),
            "member" => Some(Self::Member),
            _ => None,
        }
    }

    #[allow(dead_code)] // used by gateway; kept on both route_config copies
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Ip => "ip",
            Self::User => "user",
            Self::Member => "member",
        }
    }
}
