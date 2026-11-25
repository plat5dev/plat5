//! The config's JSON Schema, from the repo's `schema/`, compiled in. It is the
//! one definition of the config's shape (docs/concepts/config.md#schema): every
//! apply is checked against it before the cross-document rules.

use std::sync::OnceLock;

use jsonschema::{Registry, Validator};
use serde::Serialize;
use serde_json::{json, Map, Value};

const BASE: &str = "https://plat5.com/schema/";

/// Each schema file, by name, as served at `GET /schema`.
pub const FILES: &[(&str, &str)] = &[
    (
        "config.json",
        include_str!(concat!(env!("PLAT5_SCHEMA_DIR"), "/config.json")),
    ),
    (
        "plat5.json",
        include_str!(concat!(env!("PLAT5_SCHEMA_DIR"), "/plat5.json")),
    ),
    (
        "settings.json",
        include_str!(concat!(env!("PLAT5_SCHEMA_DIR"), "/settings.json")),
    ),
    (
        "kinds.json",
        include_str!(concat!(env!("PLAT5_SCHEMA_DIR"), "/kinds.json")),
    ),
    (
        "roles.json",
        include_str!(concat!(env!("PLAT5_SCHEMA_DIR"), "/roles.json")),
    ),
    (
        "authorizers.json",
        include_str!(concat!(env!("PLAT5_SCHEMA_DIR"), "/authorizers.json")),
    ),
    (
        "rate_limits.json",
        include_str!(concat!(env!("PLAT5_SCHEMA_DIR"), "/rate_limits.json")),
    ),
    (
        "cors.json",
        include_str!(concat!(env!("PLAT5_SCHEMA_DIR"), "/cors.json")),
    ),
    (
        "services.json",
        include_str!(concat!(env!("PLAT5_SCHEMA_DIR"), "/services.json")),
    ),
];

/// One problem with the config, at a field path such as
/// `services.e10s.scopes.project.routes[2].path`. The CLI adds the file.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct FieldError {
    pub path: String,
    pub message: String,
}

impl FieldError {
    pub fn new(path: impl Into<String>, message: impl Into<String>) -> Self {
        Self {
            path: path.into(),
            message: message.into(),
        }
    }
}

impl std::fmt::Display for FieldError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        if self.path.is_empty() {
            write!(f, "{}", self.message)
        } else {
            write!(f, "{}: {}", self.path, self.message)
        }
    }
}

/// Most errors one response lists.
const MAX_ERRORS: usize = 50;

struct Validators {
    config: Validator,
}

fn parsed(name: &str) -> Value {
    let (_, text) = FILES
        .iter()
        .find(|(n, _)| *n == name)
        .expect("schema file is embedded");
    serde_json::from_str(text).unwrap_or_else(|e| panic!("schema/{name}: {e}"))
}

/// A validator for `root`, with every schema file resolvable by its `$id`.
fn compile(root: &Value) -> Validator {
    let mut registry = Registry::new();
    for (name, _) in FILES {
        registry = registry
            .add(format!("{BASE}{name}"), parsed(name))
            .unwrap_or_else(|e| panic!("schema/{name}: {e}"));
    }
    let registry = registry.prepare().expect("schema registry");
    jsonschema::options()
        .offline()
        .with_registry(&registry)
        .build(root)
        .expect("schema compiles")
}

fn validators() -> &'static Validators {
    static V: OnceLock<Validators> = OnceLock::new();
    V.get_or_init(|| Validators {
        config: compile(&json!({ "$ref": format!("{BASE}config.json") })),
    })
}

/// Checks a flat config against `config.json`.
pub fn check_config(config: &Value) -> Result<(), Vec<FieldError>> {
    check(&validators().config, config)
}

fn check(validator: &Validator, instance: &Value) -> Result<(), Vec<FieldError>> {
    let mut errors: Vec<FieldError> = validator
        .iter_errors(instance)
        .map(|e| {
            FieldError::new(
                field_path(instance, e.instance_path().as_str()),
                e.to_string(),
            )
        })
        .collect();
    if errors.is_empty() {
        return Ok(());
    }
    errors.sort_by(|a, b| a.path.cmp(&b.path));
    errors.dedup();
    errors.truncate(MAX_ERRORS);
    Err(errors)
}

/// A JSON pointer into `instance` as a field path:
/// `/services/e10s/scopes/project/routes/2/path` becomes
/// `services.e10s.scopes.project.routes[2].path`.
pub fn field_path(instance: &Value, pointer: &str) -> String {
    let mut out = String::new();
    let mut at = Some(instance);
    for raw in pointer.split('/').skip(1) {
        let seg = raw.replace("~1", "/").replace("~0", "~");
        match at {
            Some(Value::Array(items)) => {
                out.push_str(&format!("[{seg}]"));
                at = seg.parse::<usize>().ok().and_then(|i| items.get(i));
            }
            other => {
                if !out.is_empty() {
                    out.push('.');
                }
                out.push_str(&seg);
                at = other.and_then(|v| v.get(&seg));
            }
        }
    }
    out
}

/// Every schema file, keyed by name, for `GET /schema` (cached once).
pub fn bundle() -> &'static Value {
    static BUNDLE: OnceLock<Value> = OnceLock::new();
    BUNDLE.get_or_init(|| {
        let mut out = Map::new();
        for (name, _) in FILES {
            out.insert(name.to_string(), parsed(name));
        }
        Value::Object(out)
    })
}

#[cfg(test)]
pub(crate) fn file(name: &str) -> Value {
    parsed(name)
}

#[cfg(test)]
mod tests;
