//! The fixtures in `fixtures/`, shared with the other services' tests:
//!
//! - `catalog/plat5.yml`: a config three kinds deep, with identity's and audit's
//!   resources at their defaults, and beside it the two feeds the control plane
//!   serves for it, `gateway-feed.json` and `identity-feed.json`. Identity's,
//!   audit's and the gateway's tests read them.
//! - `plans/<case>/`: configs and the plan each makes (`plan/tests.rs`).
//!
//! A test fails when a derived file is stale; `UPDATE_FIXTURES=1 cargo test`
//! rewrites them.

use std::path::{Path, PathBuf};

use serde_json::Value;

use crate::apply;

pub fn dir() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("fixtures")
}

pub fn yaml(path: &Path) -> Value {
    let text = std::fs::read_to_string(path).unwrap_or_else(|e| panic!("{}: {e}", path.display()));
    let yaml: serde_yaml::Value =
        serde_yaml::from_str(&text).unwrap_or_else(|e| panic!("{}: {e}", path.display()));
    serde_json::to_value(yaml).unwrap()
}

pub fn updating() -> bool {
    std::env::var("UPDATE_FIXTURES").is_ok_and(|v| v == "1")
}

/// Compares `got` with the JSON at `path`, or writes it there with
/// `UPDATE_FIXTURES=1`.
pub fn check_json(path: &Path, got: &Value) {
    if updating() {
        let text = serde_json::to_string_pretty(got).unwrap() + "\n";
        std::fs::write(path, text).unwrap_or_else(|e| panic!("{}: {e}", path.display()));
        return;
    }
    let text = std::fs::read_to_string(path).unwrap_or_else(|e| {
        panic!(
            "{}: {e} (UPDATE_FIXTURES=1 cargo test writes it)",
            path.display()
        )
    });
    let want: Value =
        serde_json::from_str(&text).unwrap_or_else(|e| panic!("{}: {e}", path.display()));
    assert!(
        *got == want,
        "{} is stale (UPDATE_FIXTURES=1 cargo test rewrites it); got:\n{}",
        path.display(),
        serde_json::to_string_pretty(got).unwrap()
    );
}

#[test]
fn catalog_feeds_are_fresh() {
    let dir = dir().join("catalog");
    let config = match apply::check(&yaml(&dir.join("plat5.yml"))) {
        Ok(c) => c,
        Err(f) => panic!("catalog/plat5.yml: {f:?}"),
    };
    check_json(
        &dir.join("gateway-feed.json"),
        &apply::gateway_body(1, &config),
    );
    check_json(
        &dir.join("identity-feed.json"),
        &apply::identity_body(1, &config),
    );
}
