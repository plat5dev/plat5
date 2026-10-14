use std::collections::HashMap;

use chrono::{DateTime, Utc};
use sqlx::postgres::PgPoolOptions;
use sqlx::{Connection, PgConnection, PgPool, Postgres, Transaction};
use tracing::info;

use crate::route_config::ServiceConfig;

const SCHEMA: &str = "control_plane";
/// The route registry's schema, renamed to `SCHEMA` on first boot.
const LEGACY_SCHEMA: &str = "routes";
const MIGRATIONS: &[(&str, &str)] = &[
    ("001_init", include_str!("../migrations/001_init.sql")),
    (
        "002_documents",
        include_str!("../migrations/002_documents.sql"),
    ),
];

#[derive(Clone)]
pub struct PgStore {
    pool: PgPool,
}

#[derive(Debug)]
pub enum PgError {
    Connect(String),
    Query(String),
    Parse(String),
}

impl std::fmt::Display for PgError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            PgError::Connect(m) | PgError::Query(m) | PgError::Parse(m) => write!(f, "{m}"),
        }
    }
}

impl std::error::Error for PgError {}

fn query_err(e: sqlx::Error) -> PgError {
    PgError::Query(e.to_string())
}

#[derive(Debug, Clone)]
pub struct Revision {
    pub service_name: String,
    pub revision: i64,
    pub config: Option<ServiceConfig>,
    pub request_id: String,
    pub created_at: DateTime<Utc>,
}

#[derive(Debug, Clone)]
pub struct CommitResult {
    pub service: String,
    pub revision: i64,
    pub config: Option<ServiceConfig>,
}

/// One revision of a document (`roles`, `cors`). The body is the stored JSON;
/// the caller owns its type.
#[derive(Debug, Clone)]
pub struct DocumentRevision {
    pub revision: i64,
    pub body: serde_json::Value,
    pub request_id: String,
    pub created_at: DateTime<Utc>,
}

/// What a gateway reads: every current service and the CORS document, at one
/// control-plane revision.
pub struct GatewaySnapshot {
    pub revision: i64,
    pub services: HashMap<String, ServiceConfig>,
    pub cors: Option<serde_json::Value>,
}

impl PgStore {
    pub async fn connect(database_url: &str) -> Result<Self, PgError> {
        if database_url.is_empty() {
            return Err(PgError::Connect("DATABASE_URL is empty".into()));
        }

        rename_legacy_schema(database_url).await?;

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
            .await
            .map_err(|e| PgError::Connect(e.to_string()))?;

        let store = Self { pool };
        store.migrate().await?;
        Ok(store)
    }

    async fn migrate(&self) -> Result<(), PgError> {
        sqlx::query(
            r#"
            CREATE TABLE IF NOT EXISTS schema_migrations (
                version TEXT PRIMARY KEY,
                applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
            )
            "#,
        )
        .execute(&self.pool)
        .await
        .map_err(query_err)?;

        for (version, sql) in MIGRATIONS {
            let mut tx = self.pool.begin().await.map_err(query_err)?;
            // Concurrent replicas booting together: one migrates, the rest wait.
            sqlx::query("LOCK TABLE schema_migrations IN EXCLUSIVE MODE")
                .execute(&mut *tx)
                .await
                .map_err(query_err)?;
            let applied: Option<(String,)> =
                sqlx::query_as("SELECT version FROM schema_migrations WHERE version = $1")
                    .bind(version)
                    .fetch_optional(&mut *tx)
                    .await
                    .map_err(query_err)?;
            if applied.is_some() {
                continue;
            }
            sqlx::raw_sql(sql)
                .execute(&mut *tx)
                .await
                .map_err(query_err)?;
            sqlx::query("INSERT INTO schema_migrations (version) VALUES ($1)")
                .bind(version)
                .execute(&mut *tx)
                .await
                .map_err(query_err)?;
            tx.commit().await.map_err(query_err)?;
            info!(version = %version, "applied control-plane migration");
        }

        Ok(())
    }

    pub async fn ping(&self) -> Result<(), PgError> {
        sqlx::query("SELECT 1")
            .execute(&self.pool)
            .await
            .map_err(query_err)?;
        Ok(())
    }

    /// The control plane's revision: bumped once by every write.
    pub async fn head(&self) -> Result<i64, PgError> {
        let (revision,): (i64,) = sqlx::query_as("SELECT revision FROM head")
            .fetch_one(&self.pool)
            .await
            .map_err(query_err)?;
        Ok(revision)
    }

    pub async fn list_current(&self) -> Result<HashMap<String, ServiceConfig>, PgError> {
        list_current_in(&self.pool).await
    }

    pub async fn get_current(&self, name: &str) -> Result<Option<(i64, ServiceConfig)>, PgError> {
        let row: Option<(i64, serde_json::Value)> = sqlx::query_as(
            r#"
            SELECT s.current_revision, r.config
            FROM services s
            JOIN revisions r
              ON r.service_name = s.name AND r.revision = s.current_revision
            WHERE s.name = $1 AND s.deleted = false
            "#,
        )
        .bind(name)
        .fetch_optional(&self.pool)
        .await
        .map_err(query_err)?;

        match row {
            Some((rev, value)) => Ok(Some((rev, parse_config(&value)?))),
            None => Ok(None),
        }
    }

    pub async fn has_row(&self, name: &str) -> Result<bool, PgError> {
        let row: Option<(bool,)> = sqlx::query_as("SELECT true FROM services WHERE name = $1")
            .bind(name)
            .fetch_optional(&self.pool)
            .await
            .map_err(query_err)?;
        Ok(row.is_some())
    }

    /// Commits every item in one transaction and bumps the head once. Returns
    /// the per-service results and the new head.
    pub async fn commit_batch(
        &self,
        items: &[(String, Option<ServiceConfig>)],
        request_id: &str,
    ) -> Result<(Vec<CommitResult>, i64), PgError> {
        let mut tx = self.pool.begin().await.map_err(query_err)?;
        let head = bump_head(&mut tx).await?;

        let mut results = Vec::with_capacity(items.len());
        for (name, config) in items {
            let result = commit_one(&mut tx, name, config.as_ref(), request_id).await?;
            results.push(result);
        }

        tx.commit().await.map_err(query_err)?;
        Ok((results, head))
    }

    pub async fn list_revisions(&self, name: &str) -> Result<Option<Vec<Revision>>, PgError> {
        if !self.has_row(name).await? {
            return Ok(None);
        }
        let rows: Vec<(i64, Option<serde_json::Value>, String, DateTime<Utc>)> = sqlx::query_as(
            r#"
            SELECT revision, config, request_id, created_at
            FROM revisions
            WHERE service_name = $1
            ORDER BY revision DESC
            "#,
        )
        .bind(name)
        .fetch_all(&self.pool)
        .await
        .map_err(query_err)?;

        let mut out = Vec::with_capacity(rows.len());
        for (revision, config, request_id, created_at) in rows {
            out.push(Revision {
                service_name: name.to_string(),
                revision,
                config: parse_optional_config(config)?,
                request_id,
                created_at,
            });
        }
        Ok(Some(out))
    }

    pub async fn get_revision(
        &self,
        name: &str,
        revision: i64,
    ) -> Result<Option<Revision>, PgError> {
        let row: Option<(Option<serde_json::Value>, String, DateTime<Utc>)> = sqlx::query_as(
            r#"
            SELECT config, request_id, created_at
            FROM revisions
            WHERE service_name = $1 AND revision = $2
            "#,
        )
        .bind(name)
        .bind(revision)
        .fetch_optional(&self.pool)
        .await
        .map_err(query_err)?;

        match row {
            Some((config, request_id, created_at)) => Ok(Some(Revision {
                service_name: name.to_string(),
                revision,
                config: parse_optional_config(config)?,
                request_id,
                created_at,
            })),
            None => Ok(None),
        }
    }

    /// The current revision of a document, or None if it was never written.
    pub async fn get_document(&self, kind: &str) -> Result<Option<DocumentRevision>, PgError> {
        get_document_in(&self.pool, kind).await
    }

    pub async fn has_document(&self, kind: &str) -> Result<bool, PgError> {
        let row: Option<(bool,)> = sqlx::query_as("SELECT true FROM documents WHERE kind = $1")
            .bind(kind)
            .fetch_optional(&self.pool)
            .await
            .map_err(query_err)?;
        Ok(row.is_some())
    }

    /// Writes a new revision of a document and bumps the head. Returns the
    /// document's new revision and the new head.
    pub async fn commit_document(
        &self,
        kind: &str,
        body: &serde_json::Value,
        request_id: &str,
    ) -> Result<(i64, i64), PgError> {
        let mut tx = self.pool.begin().await.map_err(query_err)?;
        let head = bump_head(&mut tx).await?;

        let existing: Option<(i64,)> =
            sqlx::query_as("SELECT current_revision FROM documents WHERE kind = $1 FOR UPDATE")
                .bind(kind)
                .fetch_optional(&mut *tx)
                .await
                .map_err(query_err)?;
        let next_rev = existing.map(|(r,)| r + 1).unwrap_or(1);

        sqlx::query(
            r#"
            INSERT INTO documents (kind, current_revision, updated_at)
            VALUES ($1, $2, now())
            ON CONFLICT (kind) DO UPDATE
            SET current_revision = EXCLUDED.current_revision, updated_at = now()
            "#,
        )
        .bind(kind)
        .bind(next_rev)
        .execute(&mut *tx)
        .await
        .map_err(query_err)?;

        sqlx::query(
            r#"
            INSERT INTO document_revisions (kind, revision, body, request_id, created_at)
            VALUES ($1, $2, $3, $4, now())
            "#,
        )
        .bind(kind)
        .bind(next_rev)
        .bind(body)
        .bind(request_id)
        .execute(&mut *tx)
        .await
        .map_err(query_err)?;

        tx.commit().await.map_err(query_err)?;
        Ok((next_rev, head))
    }

    pub async fn list_document_revisions(
        &self,
        kind: &str,
    ) -> Result<Option<Vec<DocumentRevision>>, PgError> {
        if !self.has_document(kind).await? {
            return Ok(None);
        }
        let rows: Vec<(i64, serde_json::Value, String, DateTime<Utc>)> = sqlx::query_as(
            r#"
            SELECT revision, body, request_id, created_at
            FROM document_revisions
            WHERE kind = $1
            ORDER BY revision DESC
            "#,
        )
        .bind(kind)
        .fetch_all(&self.pool)
        .await
        .map_err(query_err)?;
        Ok(Some(
            rows.into_iter()
                .map(
                    |(revision, body, request_id, created_at)| DocumentRevision {
                        revision,
                        body,
                        request_id,
                        created_at,
                    },
                )
                .collect(),
        ))
    }

    pub async fn get_document_revision(
        &self,
        kind: &str,
        revision: i64,
    ) -> Result<Option<DocumentRevision>, PgError> {
        let row: Option<(serde_json::Value, String, DateTime<Utc>)> = sqlx::query_as(
            r#"
            SELECT body, request_id, created_at
            FROM document_revisions
            WHERE kind = $1 AND revision = $2
            "#,
        )
        .bind(kind)
        .bind(revision)
        .fetch_optional(&self.pool)
        .await
        .map_err(query_err)?;
        Ok(row.map(|(body, request_id, created_at)| DocumentRevision {
            revision,
            body,
            request_id,
            created_at,
        }))
    }

    /// Routes and CORS at one head, read in one snapshot.
    pub async fn gateway_snapshot(&self) -> Result<GatewaySnapshot, PgError> {
        let mut tx = self.read_snapshot().await?;
        let (revision,): (i64,) = sqlx::query_as("SELECT revision FROM head")
            .fetch_one(&mut *tx)
            .await
            .map_err(query_err)?;
        let services = list_current_in(&mut *tx).await?;
        let cors = get_document_in(&mut *tx, "cors").await?.map(|d| d.body);
        tx.commit().await.map_err(query_err)?;
        Ok(GatewaySnapshot {
            revision,
            services,
            cors,
        })
    }

    /// The roles document at one head, read in one snapshot.
    pub async fn roles_snapshot(&self) -> Result<(i64, Option<serde_json::Value>), PgError> {
        let mut tx = self.read_snapshot().await?;
        let (revision,): (i64,) = sqlx::query_as("SELECT revision FROM head")
            .fetch_one(&mut *tx)
            .await
            .map_err(query_err)?;
        let roles = get_document_in(&mut *tx, "roles").await?.map(|d| d.body);
        tx.commit().await.map_err(query_err)?;
        Ok((revision, roles))
    }

    async fn read_snapshot(&self) -> Result<Transaction<'static, Postgres>, PgError> {
        let mut tx = self.pool.begin().await.map_err(query_err)?;
        sqlx::query("SET TRANSACTION ISOLATION LEVEL REPEATABLE READ, READ ONLY")
            .execute(&mut *tx)
            .await
            .map_err(query_err)?;
        Ok(tx)
    }
}

/// The route registry kept its tables in schema `routes`. Rename it once, before
/// any pooled connection creates the new schema.
async fn rename_legacy_schema(database_url: &str) -> Result<(), PgError> {
    let mut conn = PgConnection::connect(database_url)
        .await
        .map_err(|e| PgError::Connect(e.to_string()))?;
    let sql = format!(
        r#"
        DO $$
        BEGIN
            IF EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = '{LEGACY_SCHEMA}')
               AND NOT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = '{SCHEMA}') THEN
                ALTER SCHEMA {LEGACY_SCHEMA} RENAME TO {SCHEMA};
            END IF;
        END $$;
        "#
    );
    sqlx::raw_sql(&sql)
        .execute(&mut conn)
        .await
        .map_err(query_err)?;
    let _ = conn.close().await;
    Ok(())
}

async fn bump_head(tx: &mut Transaction<'_, Postgres>) -> Result<i64, PgError> {
    let (revision,): (i64,) =
        sqlx::query_as("UPDATE head SET revision = revision + 1 RETURNING revision")
            .fetch_one(&mut **tx)
            .await
            .map_err(query_err)?;
    Ok(revision)
}

async fn list_current_in<'e, E>(executor: E) -> Result<HashMap<String, ServiceConfig>, PgError>
where
    E: sqlx::Executor<'e, Database = Postgres>,
{
    let rows: Vec<(String, serde_json::Value)> = sqlx::query_as(
        r#"
        SELECT s.name, r.config
        FROM services s
        JOIN revisions r
          ON r.service_name = s.name AND r.revision = s.current_revision
        WHERE s.deleted = false
        "#,
    )
    .fetch_all(executor)
    .await
    .map_err(query_err)?;

    let mut out = HashMap::new();
    for (name, value) in rows {
        out.insert(name, parse_config(&value)?);
    }
    Ok(out)
}

async fn get_document_in<'e, E>(
    executor: E,
    kind: &str,
) -> Result<Option<DocumentRevision>, PgError>
where
    E: sqlx::Executor<'e, Database = Postgres>,
{
    let row: Option<(i64, serde_json::Value, String, DateTime<Utc>)> = sqlx::query_as(
        r#"
        SELECT d.current_revision, r.body, r.request_id, r.created_at
        FROM documents d
        JOIN document_revisions r
          ON r.kind = d.kind AND r.revision = d.current_revision
        WHERE d.kind = $1
        "#,
    )
    .bind(kind)
    .fetch_optional(executor)
    .await
    .map_err(query_err)?;
    Ok(row.map(
        |(revision, body, request_id, created_at)| DocumentRevision {
            revision,
            body,
            request_id,
            created_at,
        },
    ))
}

async fn commit_one(
    tx: &mut Transaction<'_, Postgres>,
    name: &str,
    config: Option<&ServiceConfig>,
    request_id: &str,
) -> Result<CommitResult, PgError> {
    let existing: Option<(i64,)> =
        sqlx::query_as("SELECT current_revision FROM services WHERE name = $1 FOR UPDATE")
            .bind(name)
            .fetch_optional(&mut **tx)
            .await
            .map_err(query_err)?;

    let next_rev = existing.map(|(r,)| r + 1).unwrap_or(1);
    let deleted = config.is_none();
    let json = match config {
        Some(cfg) => Some(serde_json::to_value(cfg).map_err(|e| PgError::Parse(e.to_string()))?),
        None => None,
    };

    if existing.is_some() {
        sqlx::query(
            r#"
            UPDATE services
            SET current_revision = $2, deleted = $3, updated_at = now()
            WHERE name = $1
            "#,
        )
        .bind(name)
        .bind(next_rev)
        .bind(deleted)
        .execute(&mut **tx)
        .await
        .map_err(query_err)?;
    } else {
        sqlx::query(
            r#"
            INSERT INTO services (name, current_revision, deleted, updated_at)
            VALUES ($1, $2, $3, now())
            "#,
        )
        .bind(name)
        .bind(next_rev)
        .bind(deleted)
        .execute(&mut **tx)
        .await
        .map_err(query_err)?;
    }

    sqlx::query(
        r#"
        INSERT INTO revisions (service_name, revision, config, request_id, created_at)
        VALUES ($1, $2, $3, $4, now())
        "#,
    )
    .bind(name)
    .bind(next_rev)
    .bind(json)
    .bind(request_id)
    .execute(&mut **tx)
    .await
    .map_err(query_err)?;

    Ok(CommitResult {
        service: name.to_string(),
        revision: next_rev,
        config: config.cloned(),
    })
}

fn parse_config(value: &serde_json::Value) -> Result<ServiceConfig, PgError> {
    serde_json::from_value(value.clone()).map_err(|e| PgError::Parse(e.to_string()))
}

fn parse_optional_config(
    value: Option<serde_json::Value>,
) -> Result<Option<ServiceConfig>, PgError> {
    match value {
        None | Some(serde_json::Value::Null) => Ok(None),
        Some(v) => Ok(Some(parse_config(&v)?)),
    }
}
