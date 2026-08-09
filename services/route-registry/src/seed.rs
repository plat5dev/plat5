use std::path::Path;

use crate::route_config::Config;
use tracing::{info, warn};

use crate::etcd_store::EtcdStore;

pub async fn seed_from_dir(store: &EtcdStore, dir: &str) -> Result<(), String> {
    let path = Path::new(dir);
    if !path.is_dir() {
        return Err(format!("SEED_ROUTES_DIR is not a directory: {dir}"));
    }

    let mut entries: Vec<_> = std::fs::read_dir(path)
        .map_err(|e| format!("failed to read seed dir {dir}: {e}"))?
        .filter_map(|e| e.ok())
        .filter(|e| {
            e.path()
                .extension()
                .and_then(|x| x.to_str())
                .is_some_and(|ext| ext == "yml" || ext == "yaml")
        })
        .collect();
    entries.sort_by_key(|e| e.file_name());

    if entries.is_empty() {
        warn!(dir = %dir, "no seed YAML files found");
        return Ok(());
    }

    for entry in entries {
        let file_path = entry.path();
        let content = std::fs::read_to_string(&file_path)
            .map_err(|e| format!("failed to read {}: {e}", file_path.display()))?;
        let config: Config = serde_yaml::from_str(&content)
            .map_err(|e| format!("failed to parse {}: {e}", file_path.display()))?;

        config
            .validate()
            .map_err(|e| format!("seed validation failed for {}: {e}", file_path.display()))?;

        for (name, service) in config.services {
            let prepared = service
                .prepare_for_registry(&name)
                .map_err(|e| format!("seed prepare failed for '{name}': {e}"))?;
            store
                .put(&name, &prepared)
                .await
                .map_err(|e| format!("seed put failed for '{name}': {e}"))?;
            info!(
                service = %name,
                file = %file_path.display(),
                "seeded route service"
            );
        }
    }

    Ok(())
}
