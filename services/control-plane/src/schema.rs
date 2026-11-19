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
    use std::path::{Path, PathBuf};

    use super::*;

    fn base() -> Value {
        json!({
            "kinds": {
                "organization": { "path": "/orgs/{organization}", "list_permission": "organization:read" },
                "project": {
                    "parent": "organization",
                    "path": "/projects/{project}",
                    "list_permission": "project:read"
                }
            },
            "roles": { "admin": { "permissions": ["*"], "bindable_at": ["organization", "project"] } },
            "creator_role": "admin"
        })
    }

    fn errors(config: Value) -> Vec<FieldError> {
        check_config(&config).err().unwrap_or_default()
    }

    fn has_error_at(config: Value, path: &str) -> bool {
        errors(config).iter().any(|e| e.path == path)
    }

    #[test]
    fn every_file_is_draft_2020_12() {
        for (name, _) in FILES {
            let schema = parsed(name);
            assert_eq!(
                schema["$schema"], "https://json-schema.org/draft/2020-12/schema",
                "{name}"
            );
            assert_eq!(schema["$id"], format!("{BASE}{name}"), "{name}");
            jsonschema::meta::validate(&schema).unwrap_or_else(|e| panic!("{name}: {e}"));
            compile(&json!({ "$ref": format!("{BASE}{name}") }));
        }
    }

    #[test]
    fn a_minimal_config_passes() {
        check_config(&base()).unwrap();
    }

    #[test]
    fn a_full_config_passes() {
        let mut c = base();
        c["deletion_grace"] = json!("30d");
        c["reserved_slugs"] = json!(["new", "settings", "api", "admin"]);
        c["defaults"] = json!({
            "rate_limit": ["standard"],
            "timeouts": { "connect": "10s", "read": "30s" },
            "max_body_bytes": 10485760
        });
        c["roles"]["member"] =
            json!({ "permissions": ["organization:read"], "bindable_at": ["organization"] });
        c["roles"]["developer"] = json!({
            "includes": ["member"],
            "permissions": ["project:create", "project:read", "project:write", "project:bindings:read"],
            "bindable_at": ["organization", "project"]
        });
        c["rate_limits"] = json!({
            "standard": { "requests": 60, "window": "60s" },
            "evals-org": { "requests": 300, "window": "1s", "count_at": "organization" }
        });
        c["cors"] = json!({ "allowed_origins": ["https://app.example.com"], "allowed_headers": ["x-custom"] });
        c["services"] = json!({
            "identity": { "url": "http://identity:3000", "scopes": { "user": { "routes": [
                { "path": "/user", "upstream": "/users/{user.id}", "methods": ["GET", "PATCH", "DELETE"] }
            ] } } },
            "e10s": {
                "url": "http://e10s:3000",
                "timeouts": { "connect": "2s", "read": "30s" },
                "max_body_bytes": 1048576,
                "scopes": {
                    "project": {
                        "upstream_prefix": "/projects/{project.id}",
                        "routes": [{
                            "path": "/plans/{plan_id}/versions",
                            "required_permissions": ["project:read"],
                            "methods": { "GET": null, "POST": { "required_permissions": ["catalog:write"] } }
                        }]
                    },
                    "user": { "routes": [{
                        "path": "/user/notification-settings",
                        "upstream": "/users/{user.id}/notification-settings",
                        "methods": ["GET", "PUT"]
                    }] },
                    "public": { "routes": [{ "path": "/health", "methods": ["GET"] }] }
                }
            }
        });
        check_config(&c).unwrap();
    }

    #[test]
    fn shape_errors() {
        type Mutate = Box<dyn Fn(&mut Value)>;
        let cases: Vec<(&str, Mutate)> = vec![
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
            (
                "defaults.timeouts.connect",
                Box::new(|c| c["defaults"] = json!({ "timeouts": { "connect": 10000 } })),
            ),
            (
                "defaults.max_body_bytes",
                Box::new(|c| c["defaults"] = json!({ "max_body_bytes": 0 })),
            ),
            (
                "defaults.rate_limit",
                Box::new(|c| c["defaults"] = json!({ "rate_limit": [] })),
            ),
            (
                "defaults.rate_limit",
                Box::new(|c| c["defaults"] = json!({ "rate_limit": ["standard", "standard"] })),
            ),
            (
                "kinds",
                Box::new(|c| c["kinds"]["public"] = c["kinds"]["organization"].clone()),
            ),
            (
                "kinds",
                Box::new(|c| c["kinds"]["member"] = c["kinds"]["organization"].clone()),
            ),
            (
                "kinds",
                Box::new(|c| c["kinds"]["user"] = c["kinds"]["organization"].clone()),
            ),
            (
                "kinds",
                Box::new(|c| c["kinds"]["Org"] = c["kinds"]["organization"].clone()),
            ),
            (
                "kinds.project.path",
                Box::new(|c| c["kinds"]["project"]["path"] = json!("/projects/{project}/x")),
            ),
            (
                "kinds.project.list_permission",
                Box::new(|c| c["kinds"]["project"]["list_permission"] = json!("*")),
            ),
            (
                "roles.admin.permissions",
                Box::new(|c| c["roles"]["admin"]["permissions"] = json!(["*", "x"])),
            ),
            (
                "roles.admin",
                Box::new(|c| c["roles"]["admin"] = json!({ "permissions": [] })),
            ),
            (
                "",
                Box::new(|c| {
                    c.as_object_mut().unwrap().remove("creator_role");
                }),
            ),
            (
                "rate_limits.p",
                Box::new(|c| c["rate_limits"] = json!({ "p": { "requests": 1 } })),
            ),
            (
                "rate_limits.p.window",
                Box::new(|c| c["rate_limits"] = json!({ "p": { "requests": 1, "window": 60 } })),
            ),
            (
                "rate_limits.p.requests",
                Box::new(|c| c["rate_limits"] = json!({ "p": { "requests": 0, "window": "1s" } })),
            ),
            (
                "cors.allowed_origins",
                Box::new(|c| c["cors"] = json!({ "allowed_origins": ["*", "https://a.com"] })),
            ),
            (
                "cors.allowed_origins",
                Box::new(|c| c["cors"] = json!({ "allowed_origins": ["https://a.com/"] })),
            ),
            (
                "cors.allowed_headers[0]",
                Box::new(|c| c["cors"] = json!({ "allowed_headers": ["X-Custom"] })),
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
    fn any_origin_is_written_out() {
        let mut c = base();
        c["cors"] = json!({ "allowed_origins": ["*"] });
        check_config(&c).unwrap();
        c["cors"] = json!({ "allowed_origins": [] });
        check_config(&c).unwrap();
    }

    #[test]
    fn durations() {
        for ok in ["1s", "90m", "12h", "30d"] {
            let mut c = base();
            c["deletion_grace"] = json!(ok);
            check_config(&c).unwrap();
        }
    }

    fn with_route(scope: &str, route: Value) -> Value {
        let mut c = base();
        c["services"] =
            json!({ "s": { "url": "http://s:3000", "scopes": { scope: { "routes": [route] } } } });
        c
    }

    #[test]
    fn route_shapes() {
        let at = |scope: &str| format!("services.s.scopes.{scope}.routes[0]");
        let ok = |scope: &str, route: Value| {
            check_config(&with_route(scope, route.clone()))
                .unwrap_or_else(|e| panic!("{route}: {e:?}"))
        };
        ok(
            "project",
            json!({ "path": "/", "methods": ["GET"], "required_permissions": "any_member" }),
        );
        ok(
            "project",
            json!({ "path": "/a", "methods": { "GET": null, "HEAD": { "audit": true } }, "required_permissions": ["a"] }),
        );
        for rl in [json!(false), json!(["p"]), json!(["p", "q"])] {
            ok(
                "public",
                json!({ "path": "/a", "methods": ["GET"], "rate_limit": rl }),
            );
        }
        // A route can't require "*", and [] is spelled any_member.
        let bad = with_route(
            "project",
            json!({ "path": "/a", "methods": ["GET"], "required_permissions": ["*"] }),
        );
        assert!(has_error_at(
            bad,
            &format!("{}.required_permissions", at("project"))
        ));
        let bad = with_route(
            "project",
            json!({ "path": "/a", "methods": ["GET"], "required_permissions": [] }),
        );
        assert!(has_error_at(
            bad,
            &format!("{}.required_permissions", at("project"))
        ));
        for rl in [
            json!(true),
            json!("p"),
            json!([]),
            json!({ "requests": 1, "window": "1s" }),
        ] {
            let bad = with_route(
                "public",
                json!({ "path": "/a", "methods": ["GET"], "rate_limit": rl }),
            );
            assert!(
                has_error_at(bad, &format!("{}.rate_limit", at("public"))),
                "{rl}"
            );
        }
        for methods in [
            json!(["OPTIONS"]),
            json!({ "OPTIONS": null }),
            json!([]),
            json!(["get"]),
        ] {
            let bad = with_route("public", json!({ "path": "/a", "methods": methods }));
            assert!(
                has_error_at(bad, &format!("{}.methods", at("public"))),
                "{methods}"
            );
        }
        for old in [
            json!({ "scope": "public" }),
            json!({ "resolve_deleted": true }),
            json!({ "required_labels": ["a"] }),
        ] {
            let mut route = json!({ "path": "/a", "methods": ["GET"] });
            route
                .as_object_mut()
                .unwrap()
                .extend(old.as_object().unwrap().clone());
            assert!(
                has_error_at(with_route("public", route), &at("public")),
                "{old}"
            );
        }
        let bad = with_route("public", json!({ "path": "a", "methods": ["GET"] }));
        assert!(has_error_at(bad, &format!("{}.path", at("public"))));
        let bad = with_route(
            "public",
            json!({ "path": "/a", "upstream": "/b?x=1", "methods": ["GET"] }),
        );
        assert!(has_error_at(bad, &format!("{}.upstream", at("public"))));
        // A scope needs routes; a service needs scopes.
        let mut c = base();
        c["services"] =
            json!({ "s": { "url": "http://s:3000", "scopes": { "public": { "routes": [] } } } });
        assert!(has_error_at(c, "services.s.scopes.public.routes"));
        let mut c = base();
        c["services"] = json!({ "s": { "url": "http://s:3000", "routes": [] } });
        assert!(has_error_at(c, "services.s"));
        for url in [
            "https://s:443",
            "http://s",
            "http://s:0",
            "http://s:70000",
            "http://u@s:1",
            "http://s:1/x",
        ] {
            let mut c = with_route("public", json!({ "path": "/a", "methods": ["GET"] }));
            c["services"]["s"]["url"] = json!(url);
            assert!(has_error_at(c, "services.s.url"), "{url}");
        }
    }

    fn with_service(name: &str, service: Value) -> Value {
        let mut c = base();
        c["services"] = json!({ name: service });
        c
    }

    // Identity and audit are services like any other: scopes and routes,
    // and nothing else.
    #[test]
    fn identity_and_audit_are_ordinary_services() {
        let identity = json!({ "url": "http://identity:3000", "scopes": { "organization": {
            "upstream_prefix": "/nodes/{organization.id}",
            "routes": [{ "path": "/", "methods": { "GET": { "required_permissions": ["organization:read"] } } }]
        } } });
        check_config(&with_service("identity", identity.clone())).unwrap();
        for (field, value) in [
            ("resources", json!({ "invites": false })),
            ("kinds", json!({ "project": { "resources": {} } })),
        ] {
            let mut s = identity.clone();
            s[field] = value;
            assert!(
                has_error_at(with_service("identity", s), "services.identity"),
                "{field}"
            );
        }
        // A url alone is no service.
        assert!(has_error_at(
            with_service("audit", json!({ "url": "http://audit:3002" })),
            "services.audit"
        ));
        // `{kind}` is no placeholder in a permission.
        let route = with_route(
            "project",
            json!({ "path": "/a", "methods": ["GET"], "required_permissions": ["{kind}:read"] }),
        );
        assert!(has_error_at(
            route,
            "services.s.scopes.project.routes[0].required_permissions"
        ));
    }

    fn compile_file(name: &str) -> Validator {
        compile(&json!({ "$ref": format!("{BASE}{name}") }))
    }

    #[test]
    fn a_service_file_holds_one_service() {
        let file = compile_file("services.json");
        assert!(
            file.is_valid(&json!({ "e10s": { "url": "http://e10s:3000", "scopes": {
            "public": { "routes": [{ "path": "/health", "methods": ["GET"] }] }
        } } }))
        );
        let one = json!({ "url": "http://identity:3000", "scopes": { "public": {
            "routes": [{ "path": "/health", "methods": ["GET"] }]
        } } });
        assert!(file.is_valid(&json!({ "identity": one })));
        assert!(!file.is_valid(&json!({})));
        assert!(!file.is_valid(&json!({ "identity": one, "audit": one })));
    }

    #[test]
    fn plat5_yml_lists_service_files() {
        let file = compile_file("plat5.json");
        let mut c = base();
        c["services"] = json!(["services/identity.yml", "services/e10s.yml"]);
        assert!(file.is_valid(&c));
        c["services"] = json!({ "e10s": { "url": "http://e10s:3000" } });
        assert!(!file.is_valid(&c));
    }

    fn fixtures() -> PathBuf {
        Path::new(env!("CARGO_MANIFEST_DIR")).join("fixtures")
    }

    fn yaml(path: &Path) -> Value {
        let text =
            std::fs::read_to_string(path).unwrap_or_else(|e| panic!("{}: {e}", path.display()));
        let yaml: serde_yaml::Value = serde_yaml::from_str(&text).unwrap();
        serde_json::to_value(yaml).unwrap()
    }

    #[test]
    fn fixture_configs_pass() {
        check_config(&crate::fixtures::flat(
            &fixtures().join("catalog/plat5.yml"),
        ))
        .unwrap_or_else(|e| panic!("catalog: {e:?}"));
        let mut files = Vec::new();
        for case in std::fs::read_dir(fixtures().join("plans")).unwrap() {
            let case = case.unwrap().path();
            for name in ["before.yml", "after.yml"] {
                if case.join(name).exists() {
                    files.push(case.join(name));
                }
            }
        }
        assert!(files.len() > 10, "{files:?}");
        for path in files {
            check_config(&yaml(&path)).unwrap_or_else(|e| panic!("{}: {e:?}", path.display()));
        }
    }

    #[test]
    fn field_paths() {
        let v = json!({ "a": [ { "b": 1 } ] });
        assert_eq!(field_path(&v, "/a/0/b"), "a[0].b");
        assert_eq!(field_path(&v, ""), "");
    }
}
