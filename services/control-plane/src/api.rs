use std::time::Duration;

use axum::extract::rejection::{PathRejection, QueryRejection};
use axum::extract::{Extension, Path, Query, State};
use axum::http::{header, StatusCode};
use axum::middleware as axum_mw;
use axum::response::{IntoResponse, Response};
use axum::routing::{get, post};
use axum::{Json, Router};
use serde::Deserialize;
use serde_json::{json, Value};

use crate::apply::{self, Failure};
use crate::error::AppError;
use crate::metrics;
use crate::middleware::{
    admin_auth_middleware, feed_observability, http_observability, with_request_id, RequestId,
};
use crate::schema;
use crate::served;
use crate::store::{Reader, Report};
use crate::AppState;

/// How long an internal read with `after` waits for a newer revision.
const LONG_POLL_WAIT: Duration = Duration::from_secs(25);

/// How long a gateway replica stays in `GET /status` after its last poll.
const REPLICA_SEEN_SECS: i64 = 60;

pub fn public_router(state: AppState) -> Router {
    let authed = Router::new()
        .route("/apply", post(apply_config))
        .route("/config", get(get_config))
        .route("/revisions", get(list_revisions))
        .route("/revisions/{rev}", get(get_revision))
        .route("/status", get(get_status))
        .layer(axum_mw::from_fn_with_state(
            state.clone(),
            admin_auth_middleware,
        ));

    Router::new()
        .merge(authed)
        .route("/schema", get(get_schema))
        .route_layer(axum_mw::from_fn(http_observability))
        .with_state(state)
}

pub fn internal_router(state: AppState) -> Router {
    // What the runtime reads. No token: the internal port is reachable only
    // inside the deployment's network.
    let feeds = Router::new()
        .route("/internal/gateway", get(gateway_feed))
        .route("/internal/identity", get(identity_feed))
        .route_layer(axum_mw::from_fn(feed_observability));

    Router::new()
        .route("/health/live", get(health_live))
        .route("/health/ready", get(health_ready))
        .route("/metrics", get(metrics_scrape))
        .merge(feeds)
        .with_state(state)
}

async fn metrics_scrape() -> impl IntoResponse {
    let (content_type, body) = metrics::gather_text();
    (StatusCode::OK, [(header::CONTENT_TYPE, content_type)], body)
}

async fn health_live() -> impl IntoResponse {
    Json(json!({ "status": "healthy" }))
}

async fn health_ready(State(state): State<AppState>) -> impl IntoResponse {
    if state.store.ping().await.is_ok() {
        (StatusCode::OK, Json(json!({ "status": "healthy" }))).into_response()
    } else {
        (
            StatusCode::SERVICE_UNAVAILABLE,
            Json(json!({ "status": "unhealthy" })),
        )
            .into_response()
    }
}

fn failure(request_id: &str, f: Failure) -> AppError {
    let request_id = request_id.to_string();
    match f {
        Failure::Invalid(fields) => AppError::validation(request_id, fields),
        Failure::Stale {
            precondition,
            expected,
            actual,
        } => AppError::plan_stale(request_id, precondition, expected, actual),
        Failure::CountsUnavailable(e) => {
            tracing::error!(error = %e, "plan counts unavailable");
            AppError::service_unavailable(request_id)
        }
        Failure::Store(e) => {
            tracing::error!(error = %e, "postgres failed");
            AppError::service_unavailable(request_id)
        }
    }
}

/// A body in YAML (by content type) or JSON. An empty body is **400**.
fn parse_body(
    headers: &axum::http::HeaderMap,
    body: &[u8],
    request_id: &str,
) -> Result<Value, AppError> {
    if body.iter().all(u8::is_ascii_whitespace) {
        return Err(AppError::invalid_request(
            request_id.to_string(),
            "The body is empty.",
        ));
    }
    let content_type = headers
        .get(header::CONTENT_TYPE)
        .and_then(|v| v.to_str().ok())
        .unwrap_or("application/json");
    let parsed = if content_type.contains("yaml") || content_type.contains("yml") {
        serde_yaml_ng::from_slice::<serde_yaml_ng::Value>(body)
            .map_err(|e| e.to_string())
            .and_then(|y| serde_json::to_value(y).map_err(|e| e.to_string()))
    } else {
        serde_json::from_slice(body).map_err(|e| e.to_string())
    };
    parsed.map_err(|e| {
        AppError::invalid_request(request_id.to_string(), format!("Malformed request: {e}"))
    })
}

#[derive(Deserialize)]
struct ApplyQuery {
    #[serde(default)]
    dry_run: bool,
}

/// `POST /apply?dry_run=true` answers the plan, with every service expanded as
/// the gateway feed would serve it; `POST /apply` applies it if its
/// preconditions hold (docs/concepts/config.md#plan-and-apply).
async fn apply_config(
    State(state): State<AppState>,
    Extension(RequestId(request_id)): Extension<RequestId>,
    query: Result<Query<ApplyQuery>, QueryRejection>,
    headers: axum::http::HeaderMap,
    body: axum::body::Bytes,
) -> Result<Response, AppError> {
    let Query(query) =
        query.map_err(|e| AppError::invalid_request(request_id.clone(), e.body_text()))?;
    let body = parse_body(&headers, &body, &request_id)?;
    let prepared = apply::prepare(body).map_err(|f| failure(&request_id, f))?;
    if query.dry_run {
        let dry_run = apply::dry_run(&state.store, state.counts.as_ref(), &prepared, &request_id)
            .await
            .map_err(|f| failure(&request_id, f))?;
        return Ok((with_request_id(request_id), Json(dry_run)).into_response());
    }
    let applied = apply::apply(&state.store, state.counts.as_ref(), &prepared, &request_id)
        .await
        .map_err(|f| failure(&request_id, f))?;
    state.feed.advance(applied.revision);
    Ok((
        with_request_id(request_id),
        Json(json!({ "revision": applied.revision, "summary": applied.summary })),
    )
        .into_response())
}

/// A revision's source, for a pull. Read strictly first: a source the
/// running version can't read is **422**, never served with a field dropped.
fn revision_body(r: &crate::store::Revision, request_id: &str) -> Result<Value, AppError> {
    apply::read(&r.config).map_err(|f| failure(request_id, f))?;
    Ok(json!({
        "revision": r.revision,
        "created_at": r.created_at.to_rfc3339(),
        "request_id": r.request_id,
        "summary": r.summary,
        "config": r.config,
    }))
}

/// The current config, as applied. **404** before the first apply.
async fn get_config(
    State(state): State<AppState>,
    Extension(RequestId(request_id)): Extension<RequestId>,
) -> Result<Response, AppError> {
    let current = state
        .store
        .current()
        .await
        .map_err(|e| failure(&request_id, Failure::Store(e)))?;
    match current {
        Some(r) => {
            let body = revision_body(&r, &request_id)?;
            Ok((with_request_id(request_id), Json(body)).into_response())
        }
        None => Err(AppError::not_found(request_id, "config", "head")),
    }
}

async fn list_revisions(
    State(state): State<AppState>,
    Extension(RequestId(request_id)): Extension<RequestId>,
) -> Result<Response, AppError> {
    let revisions = state
        .store
        .revisions()
        .await
        .map_err(|e| failure(&request_id, Failure::Store(e)))?;
    let revisions: Vec<Value> = revisions
        .iter()
        .map(|r| {
            json!({
                "revision": r.revision,
                "created_at": r.created_at.to_rfc3339(),
                "request_id": r.request_id,
                "summary": r.summary,
            })
        })
        .collect();
    Ok((
        with_request_id(request_id),
        Json(json!({ "revisions": revisions })),
    )
        .into_response())
}

async fn get_revision(
    State(state): State<AppState>,
    Extension(RequestId(request_id)): Extension<RequestId>,
    rev: Result<Path<i64>, PathRejection>,
) -> Result<Response, AppError> {
    let Path(rev) =
        rev.map_err(|e| AppError::invalid_request(request_id.clone(), e.body_text()))?;
    let revision = state
        .store
        .revision(rev)
        .await
        .map_err(|e| failure(&request_id, Failure::Store(e)))?;
    match revision {
        Some(r) => {
            let body = revision_body(&r, &request_id)?;
            Ok((with_request_id(request_id), Json(body)).into_response())
        }
        None => Err(AppError::not_found(
            request_id,
            "revision",
            &rev.to_string(),
        )),
    }
}

/// Where each gateway replica stands: those that polled in the last minute.
async fn get_status(
    State(state): State<AppState>,
    Extension(RequestId(request_id)): Extension<RequestId>,
) -> Result<Response, AppError> {
    let head = state
        .store
        .head()
        .await
        .map_err(|e| failure(&request_id, Failure::Store(e)))?;
    let replicas = state
        .store
        .replicas(REPLICA_SEEN_SECS)
        .await
        .map_err(|e| failure(&request_id, Failure::Store(e)))?;
    let gateway: Vec<Value> = replicas
        .iter()
        .map(|r| {
            let refused = r.report.refused.map(|revision| {
                json!({ "revision": revision, "reason": r.report.reason.clone().unwrap_or_default() })
            });
            json!({
                "replica": r.report.replica,
                "loaded": r.report.loaded,
                "refused": refused,
                "seen_at": r.seen_at.to_rfc3339(),
            })
        })
        .collect();
    Ok((
        with_request_id(request_id),
        Json(json!({ "revision": head, "gateway": gateway })),
    )
        .into_response())
}

async fn get_schema(Extension(RequestId(request_id)): Extension<RequestId>) -> Response {
    (with_request_id(request_id), Json(schema::bundle())).into_response()
}

#[derive(Deserialize)]
struct FeedQuery {
    after: Option<i64>,
}

/// The gateway's long poll also reports where its replica stands.
#[derive(Deserialize)]
struct GatewayQuery {
    after: Option<i64>,
    replica: Option<String>,
    loaded: Option<i64>,
    refused: Option<i64>,
    reason: Option<String>,
}

/// With `after`, waits until the control plane differs from it (advanced or
/// reset). False means nothing newer came within the wait or shutdown fired.
async fn wait_for_newer(state: &AppState, after: Option<i64>) -> bool {
    let Some(after) = after else {
        return true;
    };
    state.feed.wait_past(after, LONG_POLL_WAIT).await
}

/// A feed read: the body stored with the head for one reader, as it was
/// expanded at apply. **204** when nothing newer came within the wait; **404**
/// before the first apply.
async fn feed(
    state: AppState,
    after: Option<i64>,
    request_id: String,
    reader: Reader,
) -> Result<Response, AppError> {
    if !wait_for_newer(&state, after).await {
        return Ok(StatusCode::NO_CONTENT.into_response());
    }
    let current = state
        .store
        .current_body(reader)
        .await
        .map_err(|e| failure(&request_id, Failure::Store(e)))?;
    let Some((revision, body)) = current else {
        return Err(AppError::not_found(request_id, "config", "head"));
    };
    Ok(Json(served::feed_body(revision, &body)).into_response())
}

async fn gateway_feed(
    State(state): State<AppState>,
    Extension(RequestId(request_id)): Extension<RequestId>,
    Query(query): Query<GatewayQuery>,
) -> Result<Response, AppError> {
    if let Some(replica) = query.replica.filter(|r| !r.is_empty()) {
        let report = Report {
            replica,
            loaded: query.loaded.unwrap_or(0),
            refused: query.refused,
            reason: query.refused.and(query.reason),
        };
        // A report that can't be kept doesn't stop the feed.
        if let Err(e) = state.store.report(&report).await {
            tracing::warn!(error = %e, replica = %report.replica, "gateway report not kept");
        }
    }
    feed(state, query.after, request_id, Reader::Gateway).await
}

async fn identity_feed(
    State(state): State<AppState>,
    Extension(RequestId(request_id)): Extension<RequestId>,
    Query(query): Query<FeedQuery>,
) -> Result<Response, AppError> {
    feed(state, query.after, request_id, Reader::Identity).await
}
