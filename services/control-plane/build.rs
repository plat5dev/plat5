//! The config's JSON Schema lives once, at the repo's `schema/`, and is
//! compiled in. `PLAT5_SCHEMA_DIR` points elsewhere when the repo layout isn't
//! there (the Docker build copies it in).

use std::path::PathBuf;

fn main() {
    let dir = match std::env::var("PLAT5_SCHEMA_DIR") {
        Ok(dir) if !dir.is_empty() => PathBuf::from(dir),
        _ => PathBuf::from(std::env::var("CARGO_MANIFEST_DIR").unwrap()).join("../../schema"),
    };
    let dir = dir
        .canonicalize()
        .unwrap_or_else(|e| panic!("schema dir {}: {e}", dir.display()));
    println!("cargo:rerun-if-env-changed=PLAT5_SCHEMA_DIR");
    println!("cargo:rerun-if-changed={}", dir.display());
    println!("cargo:rustc-env=PLAT5_SCHEMA_DIR={}", dir.display());
}
