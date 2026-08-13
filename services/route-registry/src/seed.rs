use std::path::Path;

use tracing::{info, warn};

use crate::etcd_store::EtcdStore;
use crate::pg_store::PgStore;
use crate::projection;
use crate::route_config::Config;

pub async fn seed_missing(pg: &PgStore, etcd: &EtcdStore, dir: &str) -> Result<(), String> {
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

        let mut batch = Vec::new();
        for (name, service) in config.services {
            if pg
                .has_row(&name)
                .await
                .map_err(|e| format!("seed lookup failed for '{name}': {e}"))?
            {
                info!(
                    service = %name,
                    file = %file_path.display(),
                    "seed skipped; service already has history"
                );
                continue;
            }
            let prepared = service
                .prepare_for_registry(&name)
                .map_err(|e| format!("seed prepare failed for '{name}': {e}"))?;
            batch.push((name, Some(prepared)));
        }

        if batch.is_empty() {
            continue;
        }

        let commits = pg
            .commit_batch(&batch, "seed")
            .await
            .map_err(|e| format!("seed commit failed: {e}"))?;
        projection::project_commits(etcd, &commits).await;
        for commit in commits {
            info!(
                service = %commit.service,
                revision = commit.revision,
                file = %file_path.display(),
                "seeded route service"
            );
        }
    }

    Ok(())
}
