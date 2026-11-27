//! Revisions in Postgres, schema `control_plane`: one row per apply, and the head;
//! and each gateway replica's latest report.
//! An apply plans against an unlocked snapshot and then locks the head row only
//! to re-verify its precondition and write, so writes are serialized and head
//! order is commit order without holding a lock across network I/O.

use chrono::{DateTime, Utc};
use serde_json::Value;
use sqlx::postgres::PgPoolOptions;
use sqlx::{PgPool, Postgres, Transaction};
use tracing::info;

use crate::served::Served;

const SCHEMA: &str = "control_plane";
const MIGRATIONS: &[(&str, &str)] = &[
    ("001_init", include_str!("../migrations/001_init.sql")),
    (
        "002_revision_attribution",
        include_str!("../migrations/002_revision_attribution.sql"),
    ),
];

#[derive(Clone)]
pub struct Store {
    pool: PgPool,
}

#[derive(Debug)]
pub struct StoreError(pub String);

impl std::fmt::Display for StoreError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.0)
    }
}

impl std::error::Error for StoreError {}

impl From<sqlx::Error> for StoreError {
    fn from(e: sqlx::Error) -> Self {
        Self(e.to_string())
    }
}

/// Verified caller identity persisted with a revision.
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize)]
pub struct RevisionActor {
    #[serde(rename = "type")]
    pub actor_type: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub issuer: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub issuer_name: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub subject: Option<String>,
}

/// Optional caller-supplied context persisted alongside a revision (strictly
/// separate from verified `actor` attribution).
#[derive(Debug, Clone, PartialEq, Eq, Default, serde::Serialize)]
pub struct ChangeMetadata {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub reason: Option<String>,
    #[serde(rename = "ref", skip_serializing_if = "Option::is_none")]
    pub change_ref: Option<String>,
}

impl ChangeMetadata {
    pub fn is_empty(&self) -> bool {
        self.reason.is_none() && self.change_ref.is_none()
    }
}

/// One applied revision: the config as sent, and the plan's summary. The
/// bodies it serves are read on their own ([`Store::current_body`]).
#[derive(Debug, Clone)]
pub struct Revision {
    pub revision: i64,
    pub config: Value,
    pub summary: Value,
    pub request_id: String,
    pub created_at: DateTime<Utc>,
    pub actor: Option<RevisionActor>,
    pub metadata: Option<ChangeMetadata>,
}

fn build_actor(
    actor_type: Option<String>,
    issuer: Option<String>,
    issuer_name: Option<String>,
    subject: Option<String>,
) -> Option<RevisionActor> {
    actor_type.map(|actor_type| RevisionActor {
        actor_type,
        issuer,
        issuer_name,
        subject,
    })
}

fn build_metadata(reason: Option<String>, change_ref: Option<String>) -> Option<ChangeMetadata> {
    let m = ChangeMetadata { reason, change_ref };
    if m.is_empty() {
        None
    } else {
        Some(m)
    }
}

/// The head, locked for an apply. Dropped without `write`, it rolls back.
pub struct Locked {
    tx: Transaction<'static, Postgres>,
    pub revision: i64,
}

/// Which feed's body.
#[derive(Clone, Copy, Debug)]
pub enum Reader {
    Gateway,
    Identity,
}

impl Reader {
    fn column(self) -> &'static str {
        match self {
            Reader::Gateway => "gateway",
            Reader::Identity => "identity",
        }
    }
}

impl Store {
    pub async fn connect(database_url: &str) -> Result<Self, StoreError> {
        let pool = PgPoolOptions::new()
            .max_connections(15)
            .after_connect(|conn, _meta| {
                Box::pin(async move {
                    sqlx::query(&format!("CREATE SCHEMA IF NOT EXISTS {SCHEMA}"))
                        .execute(&mut *conn)
                        .await?;
                    sqlx::query(&format!("SET search_path TO {SCHEMA}"))
                        .execute(&mut *conn)
                        .await?;
                    Ok(())
                })
            })
            .connect(database_url)
            .await?;
        let store = Self { pool };
        store.migrate().await?;
        Ok(store)
    }

    async fn migrate(&self) -> Result<(), StoreError> {
        sqlx::query(
            "CREATE TABLE IF NOT EXISTS schema_migrations (
                version TEXT PRIMARY KEY,
                applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
            )",
        )
        .execute(&self.pool)
        .await?;
        for (version, sql) in MIGRATIONS {
            let mut tx = self.pool.begin().await?;
            // Replicas booting together: one migrates, the rest wait.
            sqlx::query("LOCK TABLE schema_migrations IN EXCLUSIVE MODE")
                .execute(&mut *tx)
                .await?;
            let applied: Option<(String,)> =
                sqlx::query_as("SELECT version FROM schema_migrations WHERE version = $1")
                    .bind(version)
                    .fetch_optional(&mut *tx)
                    .await?;
            if applied.is_some() {
                continue;
            }
            sqlx::raw_sql(sql).execute(&mut *tx).await?;
            sqlx::query("INSERT INTO schema_migrations (version) VALUES ($1)")
                .bind(version)
                .execute(&mut *tx)
                .await?;
            tx.commit().await?;
            info!(version = %version, "applied control-plane migration");
        }
        Ok(())
    }

    pub async fn ping(&self) -> Result<(), StoreError> {
        sqlx::query("SELECT 1").execute(&self.pool).await?;
        Ok(())
    }

    pub async fn head(&self) -> Result<i64, StoreError> {
        let (revision,): (i64,) = sqlx::query_as("SELECT revision FROM config_head")
            .fetch_one(&self.pool)
            .await?;
        Ok(revision)
    }

    /// The head and its config, read in one snapshot. `None` before the first apply.
    pub async fn current(&self) -> Result<Option<Revision>, StoreError> {
        let row: Option<FullRevisionRow> = sqlx::query_as(
            "SELECT r.revision, r.config, r.summary, r.request_id, r.created_at,
                    r.actor_type, r.actor_issuer, r.actor_issuer_name, r.actor_subject,
                    r.change_reason, r.change_ref
             FROM config_head h
             JOIN config_revisions r ON r.revision = h.revision",
        )
        .fetch_optional(&self.pool)
        .await?;
        Ok(row.map(
            |(
                revision,
                config,
                summary,
                request_id,
                created_at,
                actor_type,
                actor_issuer,
                actor_issuer_name,
                actor_subject,
                change_reason,
                change_ref,
            )| Revision {
                revision,
                config,
                summary,
                request_id,
                created_at,
                actor: build_actor(actor_type, actor_issuer, actor_issuer_name, actor_subject),
                metadata: build_metadata(change_reason, change_ref),
            },
        ))
    }

    /// The bodies the head serves, read in one snapshot. `None` before the
    /// first apply.
    pub async fn served(&self) -> Result<(i64, Option<Served>), StoreError> {
        let (head, gateway, identity): (i64, Option<Value>, Option<Value>) = sqlx::query_as(
            "SELECT h.revision, r.gateway, r.identity
             FROM config_head h
             LEFT JOIN config_revisions r ON r.revision = h.revision",
        )
        .fetch_one(&self.pool)
        .await?;
        let served = gateway
            .zip(identity)
            .map(|(gateway, identity)| Served { gateway, identity });
        Ok((head, served))
    }

    /// The head and one feed's body, as stored at apply, in one snapshot.
    /// `None` before the first apply.
    pub async fn current_body(&self, reader: Reader) -> Result<Option<(i64, Value)>, StoreError> {
        let row: Option<(i64, Value)> = sqlx::query_as(&format!(
            "SELECT r.revision, r.{} FROM config_head h
             JOIN config_revisions r ON r.revision = h.revision",
            reader.column()
        ))
        .fetch_optional(&self.pool)
        .await?;
        Ok(row)
    }

    /// Locks the head for an apply.
    pub async fn lock(&self) -> Result<Locked, StoreError> {
        let mut tx = self.pool.begin().await?;
        let (revision,): (i64,) = sqlx::query_as("SELECT revision FROM config_head FOR UPDATE")
            .fetch_one(&mut *tx)
            .await?;
        Ok(Locked { tx, revision })
    }

    /// Writes the next revision, its source and the bodies it serves, and
    /// moves the head to it. Returns the revision.
    #[allow(clippy::too_many_arguments)]
    pub async fn write(
        &self,
        mut locked: Locked,
        config: &Value,
        served: &Served,
        summary: &Value,
        request_id: &str,
        actor: Option<&RevisionActor>,
        metadata: Option<&ChangeMetadata>,
    ) -> Result<i64, StoreError> {
        let next = locked.revision + 1;
        sqlx::query(
            "INSERT INTO config_revisions (
                 revision, config, gateway, identity, summary, request_id,
                 actor_type, actor_issuer, actor_issuer_name, actor_subject,
                 change_reason, change_ref
             )
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)",
        )
        .bind(next)
        .bind(config)
        .bind(&served.gateway)
        .bind(&served.identity)
        .bind(summary)
        .bind(request_id)
        .bind(actor.map(|a| a.actor_type.as_str()))
        .bind(actor.and_then(|a| a.issuer.as_deref()))
        .bind(actor.and_then(|a| a.issuer_name.as_deref()))
        .bind(actor.and_then(|a| a.subject.as_deref()))
        .bind(metadata.and_then(|m| m.reason.as_deref()))
        .bind(metadata.and_then(|m| m.change_ref.as_deref()))
        .execute(&mut *locked.tx)
        .await?;
        sqlx::query("UPDATE config_head SET revision = $1")
            .bind(next)
            .execute(&mut *locked.tx)
            .await?;
        locked.tx.commit().await?;
        Ok(next)
    }

    /// History, newest first, without the configs.
    pub async fn revisions(&self) -> Result<Vec<Revision>, StoreError> {
        let rows: Vec<SummaryRevisionRow> = sqlx::query_as(
            "SELECT revision, summary, request_id, created_at,
                    actor_type, actor_issuer, actor_issuer_name, actor_subject,
                    change_reason, change_ref
             FROM config_revisions ORDER BY revision DESC",
        )
        .fetch_all(&self.pool)
        .await?;
        Ok(rows
            .into_iter()
            .map(
                |(
                    revision,
                    summary,
                    request_id,
                    created_at,
                    actor_type,
                    actor_issuer,
                    actor_issuer_name,
                    actor_subject,
                    change_reason,
                    change_ref,
                )| Revision {
                    revision,
                    config: Value::Null,
                    summary,
                    request_id,
                    created_at,
                    actor: build_actor(actor_type, actor_issuer, actor_issuer_name, actor_subject),
                    metadata: build_metadata(change_reason, change_ref),
                },
            )
            .collect())
    }

    pub async fn revision(&self, revision: i64) -> Result<Option<Revision>, StoreError> {
        let row: Option<RevisionByIdRow> = sqlx::query_as(
            "SELECT config, summary, request_id, created_at,
                    actor_type, actor_issuer, actor_issuer_name, actor_subject,
                    change_reason, change_ref
             FROM config_revisions WHERE revision = $1",
        )
        .bind(revision)
        .fetch_optional(&self.pool)
        .await?;
        Ok(row.map(
            |(
                config,
                summary,
                request_id,
                created_at,
                actor_type,
                actor_issuer,
                actor_issuer_name,
                actor_subject,
                change_reason,
                change_ref,
            )| Revision {
                revision,
                config,
                summary,
                request_id,
                created_at,
                actor: build_actor(actor_type, actor_issuer, actor_issuer_name, actor_subject),
                metadata: build_metadata(change_reason, change_ref),
            },
        ))
    }

    /// Keeps a gateway replica's latest report, seen now.
    pub async fn report(&self, report: &Report) -> Result<(), StoreError> {
        sqlx::query(
            "INSERT INTO gateway_replicas (replica, loaded, refused, reason, seen_at)
             VALUES ($1, $2, $3, $4, now())
             ON CONFLICT (replica) DO UPDATE
             SET loaded = $2, refused = $3, reason = $4, seen_at = now()",
        )
        .bind(&report.replica)
        .bind(report.loaded)
        .bind(report.refused)
        .bind(&report.reason)
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    /// The gateway replicas that reported in the last `within_secs` seconds,
    /// by replica id.
    pub async fn replicas(&self, within_secs: i64) -> Result<Vec<Replica>, StoreError> {
        let rows: Vec<ReplicaRow> = sqlx::query_as(
            "SELECT replica, loaded, refused, reason, seen_at FROM gateway_replicas
                 WHERE seen_at > now() - make_interval(secs => $1)
                 ORDER BY replica",
        )
        .bind(within_secs as f64)
        .fetch_all(&self.pool)
        .await?;
        Ok(rows
            .into_iter()
            .map(|(replica, loaded, refused, reason, seen_at)| Replica {
                report: Report {
                    replica,
                    loaded,
                    refused,
                    reason,
                },
                seen_at,
            })
            .collect())
    }

    /// Deletes gateway replica reports older than an hour.
    pub async fn prune_stale_replicas(&self) -> Result<u64, StoreError> {
        let result =
            sqlx::query("DELETE FROM gateway_replicas WHERE seen_at < now() - interval '1 hour'")
                .execute(&self.pool)
                .await?;
        Ok(result.rows_affected())
    }

    #[cfg(test)]
    pub(crate) fn lazy_for_test() -> Self {
        let pool = PgPoolOptions::new()
            .connect_lazy("postgres://127.0.0.1:1/unused")
            .expect("lazy pool");
        Self { pool }
    }
}

type ReplicaRow = (String, i64, Option<i64>, Option<String>, DateTime<Utc>);
type FullRevisionRow = (
    i64,
    Value,
    Value,
    String,
    DateTime<Utc>,
    Option<String>,
    Option<String>,
    Option<String>,
    Option<String>,
    Option<String>,
    Option<String>,
);
type SummaryRevisionRow = (
    i64,
    Value,
    String,
    DateTime<Utc>,
    Option<String>,
    Option<String>,
    Option<String>,
    Option<String>,
    Option<String>,
    Option<String>,
);
type RevisionByIdRow = (
    Value,
    Value,
    String,
    DateTime<Utc>,
    Option<String>,
    Option<String>,
    Option<String>,
    Option<String>,
    Option<String>,
    Option<String>,
);

/// What a gateway replica says on its long poll: the revision it serves, and
/// the newest one it was served if it refused it, with why.
#[derive(Debug, Clone, PartialEq)]
pub struct Report {
    pub replica: String,
    pub loaded: i64,
    pub refused: Option<i64>,
    pub reason: Option<String>,
}

#[derive(Debug, Clone)]
pub struct Replica {
    pub report: Report,
    pub seen_at: DateTime<Utc>,
}
