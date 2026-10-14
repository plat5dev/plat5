use std::path::Path;

use tracing::{info, warn};

use crate::documents::{Document, Roles};
use crate::pg_store::PgStore;
use crate::route_config::Config;

/// Dev and first boot: applies each YAML file's services that have no history.
pub async fn seed_missing(pg: &PgStore, dir: &str) -> Result<(), String> {
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

        let (commits, _) = pg
            .commit_batch(&batch, "seed")
            .await
            .map_err(|e| format!("seed commit failed: {e}"))?;
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

/// First boot: writes the roles file as the roles document when there has never
/// been one. A document with history is never overwritten; change it with
/// `PUT /roles`.
pub async fn seed_roles(pg: &PgStore, file: &str) -> Result<(), String> {
    if pg
        .has_document(Roles::KIND)
        .await
        .map_err(|e| format!("seed lookup failed for roles: {e}"))?
    {
        info!(file = %file, "roles seed skipped; roles already have history");
        return Ok(());
    }
    let content =
        std::fs::read_to_string(file).map_err(|e| format!("failed to read {file}: {e}"))?;
    let roles: Roles =
        serde_yaml::from_str(&content).map_err(|e| format!("failed to parse {file}: {e}"))?;
    roles
        .validate()
        .map_err(|e| format!("seed validation failed for {file}: {e}"))?;
    let body = serde_json::to_value(&roles).map_err(|e| e.to_string())?;
    let (revision, _) = pg
        .commit_document(Roles::KIND, &body, "seed")
        .await
        .map_err(|e| format!("roles seed commit failed: {e}"))?;
    info!(file = %file, revision, "seeded roles");
    Ok(())
}
