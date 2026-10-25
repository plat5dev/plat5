use std::time::{Duration, Instant};

use axum::extract::{MatchedPath, Path, Query, Request, State};
use axum::http::{header, HeaderValue, StatusCode};
use axum::middleware::{self, Next};
use axum::response::{IntoResponse, Response};
use axum::routing::{get, post};
use axum::{Json, Router};
use opentelemetry::trace::Status;
use serde::Deserialize;
use serde_json::{json, Value};
use tracing::Instrument;
use tracing_opentelemetry::OpenTelemetrySpanExt;
use uuid::Uuid;

use subtle::ConstantTimeEq;

use crate::apply::{self, Failure};
use crate::error::AppError;
use crate::metrics;
use crate::schema;
use crate::store::Report;
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
        .layer(middleware::from_fn_with_state(
            state.clone(),
            admin_auth_middleware,
        ));

    Router::new()
        .merge(authed)
        .route("/schema", get(get_schema))
        .route_layer(middleware::from_fn(http_observability))
        .with_state(state)
}

pub fn internal_router(state: AppState) -> Router {
    // What the runtime reads. No token: the internal port is reachable only
    // inside the deployment's network.
    let feeds = Router::new()
        .route("/internal/gateway", get(gateway_feed))
        .route("/internal/identity", get(identity_feed))
        .route_layer(middleware::from_fn(feed_observability));

    Router::new()
        .route("/health/live", get(health_live))
        .route("/health/ready", get(health_ready))
        .route("/metrics", get(metrics_scrape))
        .merge(feeds)
        .with_state(state)
}

async fn http_observability(request: Request, next: Next) -> Response {
    let start = Instant::now();
    let method = request.method().as_str().to_string();
    let path = request.uri().path().to_string();
    let route = request
        .extensions()
        .get::<MatchedPath>()
        .map(|m| m.as_str().to_string());
    let span_name = match route.as_deref() {
        Some(r) => format!("{method} {r}"),
        None => method.clone(),
    };
    let request_id = request_id_from_headers(request.headers());

    let span = tracing::info_span!(
        "http",
        otel.kind = "server",
        otel.name = %span_name,
        http.request.method = %method,
        url.path = %path,
        http.route = tracing::field::Empty,
        request_id = %request_id,
        http.response.status_code = tracing::field::Empty,
        error.kind = tracing::field::Empty,
    );
    if let Some(ref r) = route {
        span.record("http.route", r.as_str());
    }

    async move {
        let response = next.run(request).await;
        let status = response.status().as_u16();
        let duration_secs = start.elapsed().as_secs_f64();
        let duration_ms = duration_secs * 1000.0;
        let route_label = route.as_deref().unwrap_or("unmatched");

        metrics::record_request(route_label, &method, status, duration_secs);

        tracing::Span::current().record("http.response.status_code", status);

        if status >= 500 {
            let error_kind = if status == 503 { "network" } else { "internal" };
            tracing::Span::current().record("error.kind", error_kind);
            tracing::Span::current().set_status(Status::error(""));
            tracing::error!(
                route = %route_label,
                method = %method,
                status = status,
                duration_ms = duration_ms,
                request_id = %request_id,
                error_kind = error_kind,
                error_message = "request failed",
                "request completed"
            );
        } else {
            tracing::info!(
                route = %route_label,
                method = %method,
                status = status,
                duration_ms = duration_ms,
                request_id = %request_id,
                "request completed"
            );
        }

        response
    }
    .instrument(span)
    .await
}

/// A feed read is a long-poll: up to 25 seconds, over and over, per reader. As a
/// trace or a request metric it would be one long span every cycle and a latency
/// that measures nothing. Only failures are logged.
async fn feed_observability(request: Request, next: Next) -> Response {
    let route = request
        .extensions()
        .get::<MatchedPath>()
        .map(|m| m.as_str().to_string());
    let request_id = request_id_from_headers(request.headers());
    let response = next.run(request).await;
    let status = response.status().as_u16();
    if status >= 500 {
        let error_kind = if status == 503 { "network" } else { "internal" };
        tracing::error!(
            route = %route.as_deref().unwrap_or("unmatched"),
            method = "GET",
            status = status,
            request_id = %request_id,
            error_kind = error_kind,
            error_message = "request failed",
            "request completed"
        );
    }
    response
}

async fn admin_auth_middleware(
    State(state): State<AppState>,
    request: Request,
    next: Next,
) -> Result<Response, AppError> {
    let request_id = request_id_from_headers(request.headers());
    let authorized = request
        .headers()
        .get(header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.strip_prefix("Bearer "))
        .is_some_and(|t| bool::from(t.as_bytes().ct_eq(state.admin_token.as_bytes())));

    if !authorized {
        return Err(AppError::unauthorized(request_id));
    }

    Ok(next.run(request).await)
}

fn request_id_from_headers(headers: &axum::http::HeaderMap) -> String {
    headers
        .get("x-request-id")
        .and_then(|v| v.to_str().ok())
        .filter(|s| !s.is_empty())
        .map(|s| s.to_string())
        .unwrap_or_else(|| Uuid::new_v4().to_string())
}

fn with_request_id(request_id: String) -> [(header::HeaderName, HeaderValue); 1] {
    let val =
        HeaderValue::from_str(&request_id).unwrap_or_else(|_| HeaderValue::from_static("invalid"));
    [(header::HeaderName::from_static("x-request-id"), val)]
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
        serde_yaml::from_slice::<serde_yaml::Value>(body)
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
    Query(query): Query<ApplyQuery>,
    headers: axum::http::HeaderMap,
    body: axum::body::Bytes,
) -> Result<Response, AppError> {
    let request_id = request_id_from_headers(&headers);
    let body = parse_body(&headers, &body, &request_id)?;
    let prepared = apply::prepare(body).map_err(|f| failure(&request_id, f))?;
    if query.dry_run {
        let dry_run = apply::dry_run(&state.store, state.counts.as_ref(), &prepared)
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

fn revision_body(r: &crate::store::Revision) -> Value {
    json!({
        "revision": r.revision,
        "created_at": r.created_at.to_rfc3339(),
        "request_id": r.request_id,
        "summary": r.summary,
        "config": r.config,
    })
}

/// The current config, as applied. **404** before the first apply.
async fn get_config(
    State(state): State<AppState>,
    headers: axum::http::HeaderMap,
) -> Result<Response, AppError> {
    let request_id = request_id_from_headers(&headers);
    let current = state
        .store
        .current()
        .await
        .map_err(|e| failure(&request_id, Failure::Store(e)))?;
    match current {
        Some(r) => Ok((with_request_id(request_id), Json(revision_body(&r))).into_response()),
        None => Err(AppError::not_found(request_id, "config", "head")),
    }
}

async fn list_revisions(
    State(state): State<AppState>,
    headers: axum::http::HeaderMap,
) -> Result<Response, AppError> {
    let request_id = request_id_from_headers(&headers);
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
    Path(rev): Path<i64>,
    headers: axum::http::HeaderMap,
) -> Result<Response, AppError> {
    let request_id = request_id_from_headers(&headers);
    let revision = state
        .store
        .revision(rev)
        .await
        .map_err(|e| failure(&request_id, Failure::Store(e)))?;
    match revision {
        Some(r) => Ok((with_request_id(request_id), Json(revision_body(&r))).into_response()),
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
    headers: axum::http::HeaderMap,
) -> Result<Response, AppError> {
    let request_id = request_id_from_headers(&headers);
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

async fn get_schema(headers: axum::http::HeaderMap) -> Response {
    let request_id = request_id_from_headers(&headers);
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

/// With `after`, waits until the control plane is past it. False means nothing
/// newer came within the wait. A cursor ahead of the head (the store was reset)
/// is answered at once.
async fn wait_for_newer(state: &AppState, after: Option<i64>) -> bool {
    let Some(after) = after else {
        return true;
    };
    if after > state.feed.current() {
        return true;
    }
    state.feed.wait_past(after, LONG_POLL_WAIT).await
}

/// A feed read: the current config shaped for one reader. **204** when nothing
/// newer came within the wait; **404** before the first apply.
async fn feed(
    state: AppState,
    after: Option<i64>,
    headers: &axum::http::HeaderMap,
    shape: fn(i64, &Value) -> Result<Value, Failure>,
) -> Result<Response, AppError> {
    let request_id = request_id_from_headers(headers);
    if !wait_for_newer(&state, after).await {
        return Ok(StatusCode::NO_CONTENT.into_response());
    }
    let current = state
        .store
        .current()
        .await
        .map_err(|e| failure(&request_id, Failure::Store(e)))?;
    let Some(current) = current else {
        return Err(AppError::not_found(request_id, "config", "head"));
    };
    let body = shape(current.revision, &current.config).map_err(|f| failure(&request_id, f))?;
    Ok(Json(body).into_response())
}

async fn gateway_feed(
    State(state): State<AppState>,
    Query(query): Query<GatewayQuery>,
    headers: axum::http::HeaderMap,
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
    feed(state, query.after, &headers, apply::gateway_feed).await
}

async fn identity_feed(
    State(state): State<AppState>,
    Query(query): Query<FeedQuery>,
    headers: axum::http::HeaderMap,
) -> Result<Response, AppError> {
    feed(state, query.after, &headers, apply::identity_feed).await
}
