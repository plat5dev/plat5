use std::time::Duration;

use tracing::{error, info, warn};

use crate::etcd_store::EtcdStore;
use crate::pg_store::{CommitResult, PgStore};

pub async fn project_commits(etcd: &EtcdStore, commits: &[CommitResult]) {
    for commit in commits {
        match &commit.config {
            Some(cfg) => {
                if let Err(e) = etcd.put(&commit.service, cfg).await {
                    warn!(
                        service = %commit.service,
                        revision = commit.revision,
                        error = %e,
                        "etcd project put failed; reconciler will retry"
                    );
                }
            }
            None => {
                if let Err(e) = etcd.delete(&commit.service).await {
                    warn!(
                        service = %commit.service,
                        revision = commit.revision,
                        error = %e,
                        "etcd project delete failed; reconciler will retry"
                    );
                }
            }
        }
    }
}

pub async fn reconcile_once(pg: &PgStore, etcd: &EtcdStore) -> Result<(), String> {
    let desired = pg.list_current().await.map_err(|e| e.to_string())?;
    let live = etcd.list().await.map_err(|e| e.to_string())?;

    for (name, cfg) in &desired {
        let stale = match live.get(name) {
            Some(existing) => existing != cfg,
            None => true,
        };
        if stale {
            etcd.put(name, cfg).await.map_err(|e| e.to_string())?;
            info!(service = %name, "reconciled etcd put");
        }
    }

    for name in live.keys() {
        if !desired.contains_key(name) {
            etcd.delete(name).await.map_err(|e| e.to_string())?;
            info!(service = %name, "reconciled etcd delete");
        }
    }

    Ok(())
}

pub fn spawn_reconciler(pg: PgStore, etcd: EtcdStore) {
    tokio::spawn(async move {
        let mut interval = tokio::time::interval(Duration::from_secs(5));
        interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
        loop {
            interval.tick().await;
            if let Err(e) = reconcile_once(&pg, &etcd).await {
                error!(error = %e, "route projection reconcile failed");
            }
        }
    });
}
