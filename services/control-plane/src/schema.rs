//! The config's JSON Schema, from the repo's `schema/v2/`, compiled in. It is the
//! one definition of the config's shape (docs/concepts/config.md#schema): every
//! apply is checked against it before the cross-document rules.

use std::sync::OnceLock;

use jsonschema::{Registry, Validator};
use serde::Serialize;
use serde_json::{json, Map, Value};

const BASE: &str = "https://plat5.com/schema/v2/";

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
        "kinds.json",
        include_str!(concat!(env!("PLAT5_SCHEMA_DIR"), "/kinds.json")),
    ),
    (
        "roles.json",
        include_str!(concat!(env!("PLAT5_SCHEMA_DIR"), "/roles.json")),
    ),
    (
        "cors.json",
        include_str!(concat!(env!("PLAT5_SCHEMA_DIR"), "/cors.json")),
    ),
    (
        "routes.json",
        include_str!(concat!(env!("PLAT5_SCHEMA_DIR"), "/routes.json")),
    ),
];

/// One problem with the config, at a field path such as
/// `services.e10s.routes[2].scope`. The CLI adds the file.
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
    catalog: Validator,
}

fn parsed(name: &str) -> Value {
    let (_, text) = FILES
        .iter()
        .find(|(n, _)| *n == name)
        .expect("schema file is embedded");
    serde_json::from_str(text).unwrap_or_else(|e| panic!("schema/v2/{name}: {e}"))
}

fn validators() -> &'static Validators {
    static V: OnceLock<Validators> = OnceLock::new();
    V.get_or_init(|| {
        let mut registry = Registry::new();
        for (name, _) in FILES {
            registry = registry
                .add(format!("{BASE}{name}"), parsed(name))
                .unwrap_or_else(|e| panic!("schema/v2/{name}: {e}"));
        }
        let registry = registry.prepare().expect("schema registry");
        let build = |schema: &Value| {
            jsonschema::options()
                .offline()
                .with_registry(&registry)
                .build(schema)
                .expect("schema compiles")
        };
        // `POST /catalog` takes only the version and the kinds.
        let catalog = json!({
            "type": "object",
            "additionalProperties": false,
            "required": ["version", "kinds"],
            "properties": {
                "version": { "$ref": format!("{BASE}config.json#/properties/version") },
                "kinds": { "$ref": format!("{BASE}kinds.json#/properties/kinds") }
            }
        });
        Validators {
            config: build(&json!({ "$ref": format!("{BASE}config.json") })),
            catalog: build(&catalog),
        }
    })
}

/// Checks a flat config against `config.json`.
pub fn check_config(config: &Value) -> Result<(), Vec<FieldError>> {
    check(&validators().config, config)
}

/// Checks a `POST /catalog` body: `{ version, kinds }`.
pub fn check_catalog_request(body: &Value) -> Result<(), Vec<FieldError>> {
    check(&validators().catalog, body)
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

/// A JSON pointer into `instance` as a field path: `/services/e10s/routes/2/scope`
/// becomes `services.e10s.routes[2].scope`.
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

/// Every schema file, keyed by name, for `GET /schema`.
pub fn bundle() -> Value {
    let mut out = Map::new();
    for (name, _) in FILES {
        out.insert(name.to_string(), parsed(name));
    }
    Value::Object(out)
}

#[cfg(test)]
pub(crate) fn file(name: &str) -> Value {
    parsed(name)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn base() -> Value {
        json!({
            "version": 2,
            "kinds": {
                "directory": { "sign_in": "idp" },
                "organization": { "label": "Organization", "segment": "orgs" }
            },
            "roles": { "admin": { "labels": ["*"], "bindable_at": ["organization"] } },
            "default_role": "admin",
            "creator_role": "admin"
        })
    }

    fn errors(config: Value) -> Vec<FieldError> {
        check_config(&config).err().unwrap_or_default()
    }

    #[test]
    fn a_minimal_config_passes() {
        check_config(&base()).unwrap();
    }

    #[test]
    fn errors_name_field_paths() {
        let mut c = base();
        c["services"] = json!({ "e10s": { "url": "http://e10s:3000", "routes": [
            { "path": "/a", "scope": "public", "methods": ["GET"] },
            { "path": "/b", "methods": ["GET"] }
        ] } });
        let errs = errors(c);
        assert!(
            errs.iter()
                .any(|e| e.path == "services.e10s.routes[1]" && e.message.contains("scope")),
            "{errs:?}"
        );
    }

    #[test]
    fn shape_errors() {
        type Mutate = Box<dyn Fn(&mut Value)>;
        let cases: Vec<(&str, Mutate)> = vec![
            ("version", Box::new(|c| c["version"] = json!(3))),
            (
                "deletion_grace",
                Box::new(|c| c["deletion_grace"] = json!("30 days")),
            ),
            (
                "deletion_grace",
                Box::new(|c| c["deletion_grace"] = json!("0d")),
            ),
            (
                "reserved_slugs[0]",
                Box::new(|c| c["reserved_slugs"] = json!(["a_b"])),
            ),
            ("kinds", Box::new(|c| c["kinds"]["public"] = json!({}))),
            ("kinds", Box::new(|c| c["kinds"]["member"] = json!({}))),
            ("kinds", Box::new(|c| c["kinds"]["Org"] = json!({}))),
            (
                "kinds.organization",
                Box::new(|c| c["kinds"]["organization"]["name_claim"] = json!("name")),
            ),
            (
                "kinds.organization",
                Box::new(|c| c["kinds"]["organization"]["extra"] = json!(1)),
            ),
            (
                "roles.admin.labels",
                Box::new(|c| c["roles"]["admin"]["labels"] = json!(["*", "x"])),
            ),
            (
                "roles.admin",
                Box::new(|c| c["roles"]["admin"] = json!({ "labels": [] })),
            ),
            (
                "roles.admin.max_duration",
                Box::new(|c| c["roles"]["admin"]["max_duration"] = json!("1w")),
            ),
            (
                "cors.allowed_origins[0]",
                Box::new(|c| c["cors"] = json!({ "allowed_origins": ["https://App.example.com"] })),
            ),
            (
                "cors.allowed_origins[0]",
                Box::new(|c| c["cors"] = json!({ "allowed_origins": ["https://a.com/"] })),
            ),
            ("", Box::new(|c| c["unknown"] = json!(1))),
        ];
        for (path, mutate) in cases {
            let mut c = base();
            mutate(&mut c);
            let errs = errors(c);
            assert!(errs.iter().any(|e| e.path == path), "{path}: {errs:?}");
        }
    }

    #[test]
    fn durations() {
        for ok in ["1s", "90m", "12h", "30d"] {
            let mut c = base();
            c["deletion_grace"] = json!(ok);
            check_config(&c).unwrap();
        }
    }

    fn route_errors(route: Value) -> Vec<FieldError> {
        let mut c = base();
        c["services"] = json!({ "s": { "url": "http://s:3000", "routes": [route] } });
        errors(c)
    }

    #[test]
    fn route_shapes() {
        // Map form carries no route-level fields.
        let errs = route_errors(json!({
            "path": "/a", "scope": "public", "methods": { "GET": null }, "audit": true
        }));
        assert!(!errs.is_empty());
        // A route can't require "*".
        let errs = route_errors(json!({
            "path": "/a", "scope": "organization", "methods": ["GET"], "required_labels": ["*"]
        }));
        assert!(
            errs.iter()
                .any(|e| e.path == "services.s.routes[0].required_labels[0]"),
            "{errs:?}"
        );
        // rate_limit: true
        assert!(!route_errors(json!({
            "path": "/a", "scope": "public", "methods": ["GET"], "rate_limit": true
        }))
        .is_empty());
        // Every rate_limit form.
        for rl in [
            json!(false),
            json!("p"),
            json!(["p", "q"]),
            json!({ "requests": 1, "window_seconds": 1 }),
        ] {
            assert!(route_errors(json!({
                "path": "/a", "scope": "public", "methods": ["GET"], "rate_limit": rl
            }))
            .is_empty());
        }
        for url in [
            "https://s:443",
            "http://s",
            "http://s:0",
            "http://s:70000",
            "http://u@s:1",
            "http://s:1/x",
        ] {
            let mut c = base();
            c["services"] = json!({ "s": { "url": url, "routes": [
                { "path": "/a", "scope": "public", "methods": ["GET"] }
            ] } });
            assert!(
                errors(c).iter().any(|e| e.path == "services.s.url"),
                "{url}"
            );
        }
    }

    #[test]
    fn catalog_request() {
        check_catalog_request(&json!({ "version": 2, "kinds": base()["kinds"] })).unwrap();
        assert!(check_catalog_request(&json!({ "kinds": base()["kinds"] })).is_err());
    }

    #[test]
    fn field_paths() {
        let v = json!({ "a": [ { "b": 1 } ] });
        assert_eq!(field_path(&v, "/a/0/b"), "a[0].b");
        assert_eq!(field_path(&v, ""), "");
    }
}
