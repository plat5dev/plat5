//! Revisions in Postgres, schema `control_plane`: one row per apply, and the head;
//! and each gateway replica's latest report.
//! An apply locks the head row from the read it plans against until its write,
//! so writes are serialized and head order is commit order.

use chrono::{DateTime, Utc};
use serde_json::Value;
use sqlx::postgres::PgPoolOptions;
use sqlx::{PgPool, Postgres, Transaction};
use tracing::info;

const SCHEMA: &str = "control_plane";
const MIGRATIONS: &[(&str, &str)] = &[("001_init", include_str!("../migrations/001_init.sql"))];

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

/// One applied revision: the config as sent, and the plan's summary.
#[derive(Debug, Clone)]
pub struct Revision {
    pub revision: i64,
    pub config: Value,
    pub summary: Value,
    pub request_id: String,
    pub created_at: DateTime<Utc>,
}

/// The head, locked for an apply. Dropped without `write`, it rolls back.
pub struct Locked {
    tx: Transaction<'static, Postgres>,
    pub revision: i64,
    pub config: Option<Value>,
}

impl Store {
    pub async fn connect(database_url: &str) -> Result<Self, StoreError> {
        let pool = PgPoolOptions::new()
            .max_connections(5)
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
        let mut tx = self.pool.begin().await?;
        sqlx::query("SET TRANSACTION ISOLATION LEVEL REPEATABLE READ, READ ONLY")
            .execute(&mut *tx)
            .await?;
        let (head,): (i64,) = sqlx::query_as("SELECT revision FROM config_head")
            .fetch_one(&mut *tx)
            .await?;
        let rev = revision_in(&mut tx, head).await?;
        tx.commit().await?;
        Ok(rev)
    }

    /// Locks the head for an apply and reads the config it's at.
    pub async fn lock(&self) -> Result<Locked, StoreError> {
        let mut tx = self.pool.begin().await?;
        let (revision,): (i64,) = sqlx::query_as("SELECT revision FROM config_head FOR UPDATE")
            .fetch_one(&mut *tx)
            .await?;
        let config = revision_in(&mut tx, revision).await?.map(|r| r.config);
        Ok(Locked {
            tx,
            revision,
            config,
        })
    }

    /// Writes the next revision and moves the head to it. Returns the revision.
    pub async fn write(
        &self,
        mut locked: Locked,
        config: &Value,
        summary: &Value,
        request_id: &str,
    ) -> Result<i64, StoreError> {
        let next = locked.revision + 1;
        sqlx::query(
            "INSERT INTO config_revisions (revision, config, summary, request_id)
             VALUES ($1, $2, $3, $4)",
        )
        .bind(next)
        .bind(config)
        .bind(summary)
        .bind(request_id)
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
        let rows: Vec<(i64, Value, String, DateTime<Utc>)> = sqlx::query_as(
            "SELECT revision, summary, request_id, created_at
             FROM config_revisions ORDER BY revision DESC",
        )
        .fetch_all(&self.pool)
        .await?;
        Ok(rows
            .into_iter()
            .map(|(revision, summary, request_id, created_at)| Revision {
                revision,
                config: Value::Null,
                summary,
                request_id,
                created_at,
            })
            .collect())
    }

    pub async fn revision(&self, revision: i64) -> Result<Option<Revision>, StoreError> {
        let mut conn = self.pool.acquire().await?;
        revision_in(&mut conn, revision).await
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
}

type ReplicaRow = (String, i64, Option<i64>, Option<String>, DateTime<Utc>);

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

async fn revision_in(
    conn: &mut sqlx::PgConnection,
    revision: i64,
) -> Result<Option<Revision>, StoreError> {
    let row: Option<(Value, Value, String, DateTime<Utc>)> = sqlx::query_as(
        "SELECT config, summary, request_id, created_at
         FROM config_revisions WHERE revision = $1",
    )
    .bind(revision)
    .fetch_optional(conn)
    .await?;
    Ok(
        row.map(|(config, summary, request_id, created_at)| Revision {
            revision,
            config,
            summary,
            request_id,
            created_at,
        }),
    )
}
