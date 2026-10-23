//! The config's JSON Schema, from the repo's `schema/`, compiled in
//! (docs/concepts/config.md#schema). Every gateway feed snapshot is checked
//! against it before the gateway builds routes from it.

use std::sync::OnceLock;

use jsonschema::{Registry, Validator};
use serde_json::{json, Value};

const BASE: &str = "https://plat5.com/schema/";

const FILES: &[(&str, &str)] = &[
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

/// Most errors one refusal lists.
const MAX_ERRORS: usize = 10;

fn validator() -> &'static Validator {
    static V: OnceLock<Validator> = OnceLock::new();
    V.get_or_init(|| {
        let mut registry = Registry::new();
        for (name, text) in FILES {
            let schema: Value =
                serde_json::from_str(text).unwrap_or_else(|e| panic!("schema/{name}: {e}"));
            registry = registry
                .add(format!("{BASE}{name}"), schema)
                .unwrap_or_else(|e| panic!("schema/{name}: {e}"));
        }
        let registry = registry.prepare().expect("schema registry");
        // `GET /internal/gateway`: the config's kinds, services and CORS, plus the revision.
        let feed = json!({
            "type": "object",
            "required": ["revision", "kinds", "services", "cors"],
            "properties": {
                "revision": { "type": "integer", "minimum": 1 },
                "kinds": { "$ref": format!("{BASE}kinds.json#/properties/kinds") },
                "services": { "$ref": format!("{BASE}config.json#/properties/services") },
                "cors": { "$ref": format!("{BASE}cors.json#/properties/cors") }
            }
        });
        jsonschema::options()
            .offline()
            .with_registry(&registry)
            .build(&feed)
            .expect("schema compiles")
    })
}

/// Checks a gateway feed answer against the schema. Err lists what failed, by path.
pub fn check_gateway_feed(feed: &Value) -> Result<(), String> {
    let errors: Vec<String> = validator()
        .iter_errors(feed)
        .take(MAX_ERRORS)
        .map(|e| format!("{}: {}", e.instance_path(), e))
        .collect();
    if errors.is_empty() {
        Ok(())
    } else {
        Err(errors.join("; "))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn accepts_a_feed_and_names_what_fails() {
        let mut feed = json!({
            "revision": 1,
            "kinds": { "directory": { "sign_in": "idp" } },
            "services": {},
            "cors": { "allowed_origins": [] }
        });
        check_gateway_feed(&feed).unwrap();
        feed["kinds"]["Bad!"] = json!({});
        let err = check_gateway_feed(&feed).unwrap_err();
        assert!(err.contains("/kinds"), "{err}");
    }
}
