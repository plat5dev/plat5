use std::collections::HashMap;

use chrono::{DateTime, Utc};
use sqlx::postgres::PgPoolOptions;
use sqlx::{PgPool, Postgres, Transaction};
use tracing::info;

use crate::route_config::ServiceConfig;

const SCHEMA: &str = "routes";
const INIT_SQL: &str = include_str!("../migrations/001_init.sql");

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

impl PgStore {
    pub async fn connect(database_url: &str) -> Result<Self, PgError> {
        if database_url.is_empty() {
            return Err(PgError::Connect("DATABASE_URL is empty".into()));
        }

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
        sqlx::query(&format!("CREATE SCHEMA IF NOT EXISTS {SCHEMA}"))
            .execute(&self.pool)
            .await
            .map_err(|e| PgError::Query(e.to_string()))?;

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
        .map_err(|e| PgError::Query(e.to_string()))?;

        let applied: Option<(String,)> =
            sqlx::query_as("SELECT version FROM schema_migrations WHERE version = $1")
                .bind("001_init")
                .fetch_optional(&self.pool)
                .await
                .map_err(|e| PgError::Query(e.to_string()))?;

        if applied.is_none() {
            let mut tx = self
                .pool
                .begin()
                .await
                .map_err(|e| PgError::Query(e.to_string()))?;
            sqlx::raw_sql(INIT_SQL)
                .execute(&mut *tx)
                .await
                .map_err(|e| PgError::Query(e.to_string()))?;
            sqlx::query("INSERT INTO schema_migrations (version) VALUES ($1)")
                .bind("001_init")
                .execute(&mut *tx)
                .await
                .map_err(|e| PgError::Query(e.to_string()))?;
            tx.commit()
                .await
                .map_err(|e| PgError::Query(e.to_string()))?;
            info!("applied routes migration 001_init");
        }

        Ok(())
    }

    pub async fn ping(&self) -> Result<(), PgError> {
        sqlx::query("SELECT 1")
            .execute(&self.pool)
            .await
            .map_err(|e| PgError::Query(e.to_string()))?;
        Ok(())
    }

    pub async fn list_current(&self) -> Result<HashMap<String, ServiceConfig>, PgError> {
        let rows: Vec<(String, serde_json::Value)> = sqlx::query_as(
            r#"
            SELECT s.name, r.config
            FROM services s
            JOIN revisions r
              ON r.service_name = s.name AND r.revision = s.current_revision
            WHERE s.deleted = false
            "#,
        )
        .fetch_all(&self.pool)
        .await
        .map_err(|e| PgError::Query(e.to_string()))?;

        let mut out = HashMap::new();
        for (name, value) in rows {
            let cfg = parse_config(&value)?;
            out.insert(name, cfg);
        }
        Ok(out)
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
        .map_err(|e| PgError::Query(e.to_string()))?;

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
            .map_err(|e| PgError::Query(e.to_string()))?;
        Ok(row.is_some())
    }

    pub async fn commit_batch(
        &self,
        items: &[(String, Option<ServiceConfig>)],
        request_id: &str,
    ) -> Result<Vec<CommitResult>, PgError> {
        let mut tx = self
            .pool
            .begin()
            .await
            .map_err(|e| PgError::Query(e.to_string()))?;

        let mut results = Vec::with_capacity(items.len());
        for (name, config) in items {
            let result = commit_one(&mut tx, name, config.as_ref(), request_id).await?;
            results.push(result);
        }

        tx.commit()
            .await
            .map_err(|e| PgError::Query(e.to_string()))?;
        Ok(results)
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
        .map_err(|e| PgError::Query(e.to_string()))?;

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
        .map_err(|e| PgError::Query(e.to_string()))?;

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
            .map_err(|e| PgError::Query(e.to_string()))?;

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
        .map_err(|e| PgError::Query(e.to_string()))?;
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
        .map_err(|e| PgError::Query(e.to_string()))?;
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
    .map_err(|e| PgError::Query(e.to_string()))?;

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
