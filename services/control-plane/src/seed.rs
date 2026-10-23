//! First boot: `SEED_DIR` holds a `plat5.yml` and the routes files it lists. When
//! the head is `0` it's applied like any apply, with `expect.revision: 0`; there
//! is nothing to destroy, so no `confirm`. Never read again after.

use std::path::Path;

use serde_json::{json, Map, Value};
use tracing::info;

use crate::apply::{self, Failure};
use crate::counts::Counts;
use crate::plan::Expect;
use crate::store::Store;

pub async fn seed(store: &Store, counts: &dyn Counts, dir: &str) -> Result<Option<i64>, String> {
    let head = store.head().await.map_err(|e| format!("seed: {e}"))?;
    if head != 0 {
        info!(head, "seed skipped; the config has revisions");
        return Ok(None);
    }
    let (body, files) = assemble(Path::new(dir))?;
    let mut prepared = apply::prepare(body).map_err(|f| describe(f, &files))?;
    prepared.expect = Some(Expect { revision: 0 });
    let applied = apply::apply(store, counts, &prepared, "seed")
        .await
        .map_err(|f| describe(f, &files))?;
    info!(dir = %dir, revision = applied.revision, "seeded the config");
    Ok(Some(applied.revision))
}

/// The flat config from a `plat5.yml` and its routes files, as the CLI sends it:
/// `services` becomes a map of name to service. Also each service's file, to
/// name it in errors.
pub fn assemble(dir: &Path) -> Result<(Value, Map<String, Value>), String> {
    let main = dir.join("plat5.yml");
    let mut config = read_yaml(&main)?;
    let Value::Object(top) = &mut config else {
        return Err(format!("{}: must be a mapping", main.display()));
    };
    let listed = top.remove("services").unwrap_or(json!([]));
    let Value::Array(listed) = listed else {
        return Err(format!(
            "{}: services must be a list of routes files",
            main.display()
        ));
    };
    let mut services = Map::new();
    let mut files = Map::new();
    for entry in listed {
        let Some(rel) = entry.as_str() else {
            return Err(format!(
                "{}: services must be a list of routes files",
                main.display()
            ));
        };
        let path = dir.join(rel);
        let Value::Object(file) = read_yaml(&path)? else {
            return Err(format!(
                "{rel}: must hold exactly one service, keyed by its name"
            ));
        };
        if file.len() != 1 {
            return Err(format!(
                "{rel}: must hold exactly one service, keyed by its name"
            ));
        }
        let (name, service) = file.into_iter().next().expect("one entry");
        if let Some(other) = files.get(&name) {
            return Err(format!(
                "{rel}: service {name} is already in {}",
                other.as_str().unwrap_or("")
            ));
        }
        files.insert(name.clone(), json!(rel));
        services.insert(name, service);
    }
    top.insert("services".into(), Value::Object(services));
    Ok((config, files))
}

fn read_yaml(path: &Path) -> Result<Value, String> {
    let text = std::fs::read_to_string(path).map_err(|e| format!("{}: {e}", path.display()))?;
    let yaml: serde_yaml::Value =
        serde_yaml::from_str(&text).map_err(|e| format!("{}: {e}", path.display()))?;
    serde_json::to_value(yaml).map_err(|e| format!("{}: {e}", path.display()))
}

/// A failure, with each field path's file: `services.<name>` is that service's
/// routes file, everything else is `plat5.yml`.
fn describe(f: Failure, files: &Map<String, Value>) -> String {
    let file_of = |path: &str| -> String {
        path.strip_prefix("services.")
            .and_then(|rest| {
                files
                    .iter()
                    .filter(|(name, _)| {
                        rest == name.as_str() || rest.starts_with(&format!("{name}."))
                    })
                    .max_by_key(|(name, _)| name.len())
                    .and_then(|(_, f)| f.as_str())
            })
            .unwrap_or("plat5.yml")
            .to_string()
    };
    match f {
        Failure::Invalid(errs) => errs
            .iter()
            .map(|e| format!("{}: {e}", file_of(&e.path)))
            .collect::<Vec<_>>()
            .join("; "),
        Failure::RouteConflict(c) => format!("route owned by two services: {}", c.join("; ")),
        Failure::Stale { precondition, .. } => format!("seed raced another apply ({precondition})"),
        Failure::CountsUnavailable(e) => e,
        Failure::Store(e) => e.to_string(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn dir(files: &[(&str, &str)]) -> tempdir::Dir {
        let d = tempdir::Dir::new();
        for (name, text) in files {
            let path = d.0.join(name);
            std::fs::create_dir_all(path.parent().unwrap()).unwrap();
            std::fs::write(path, text).unwrap();
        }
        d
    }

    mod tempdir {
        pub struct Dir(pub std::path::PathBuf);
        impl Dir {
            pub fn new() -> Self {
                let p = std::env::temp_dir().join(format!("cp-seed-{}", uuid::Uuid::new_v4()));
                std::fs::create_dir_all(&p).unwrap();
                Self(p)
            }
        }
        impl Drop for Dir {
            fn drop(&mut self) {
                let _ = std::fs::remove_dir_all(&self.0);
            }
        }
    }

    const PLAT5: &str = "
version: 2
kinds:
  directory: { sign_in: idp }
  organization: { segment: orgs }
roles:
  admin: { labels: ['*'], bindable_at: [organization] }
default_role: admin
creator_role: admin
services: [routes/e10s.yml]
";

    #[test]
    fn assembles_one_flat_config() {
        let d = dir(&[
            ("plat5.yml", PLAT5),
            ("routes/e10s.yml", "e10s:\n  url: http://e10s:3000\n  routes:\n    - { path: /h, scope: public, methods: [GET] }\n"),
        ]);
        let (config, files) = assemble(&d.0).unwrap();
        assert_eq!(config["services"]["e10s"]["url"], json!("http://e10s:3000"));
        assert_eq!(files["e10s"], json!("routes/e10s.yml"));
        apply::prepare(config).unwrap();
    }

    #[test]
    fn a_routes_file_holds_one_service() {
        let d = dir(&[
            ("plat5.yml", PLAT5),
            (
                "routes/e10s.yml",
                "a: { url: 'http://a:1', routes: [] }\nb: { url: 'http://b:1', routes: [] }\n",
            ),
        ]);
        assert!(assemble(&d.0).unwrap_err().contains("exactly one service"));
    }

    #[test]
    fn errors_name_the_file() {
        let d = dir(&[
            ("plat5.yml", PLAT5),
            (
                "routes/e10s.yml",
                "e10s:\n  url: http://e10s:3000\n  routes:\n    - { path: /h, methods: [GET] }\n",
            ),
        ]);
        let (config, files) = assemble(&d.0).unwrap();
        let Err(f) = apply::prepare(config) else {
            panic!()
        };
        let msg = describe(f, &files);
        assert!(
            msg.starts_with("routes/e10s.yml: services.e10s.routes[0]"),
            "{msg}"
        );
    }
}
