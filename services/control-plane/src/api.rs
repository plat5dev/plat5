use std::time::{Duration, Instant};

use axum::extract::{MatchedPath, Path, Query, Request, State};
use axum::http::{header, HeaderValue, StatusCode};
use axum::middleware::{self, Next};
use axum::response::{IntoResponse, Response};
use axum::routing::{get, post};
use axum::{Json, Router};
use opentelemetry::trace::{Status, TraceContextExt};
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
use crate::served;
use crate::store::{Reader, Report};
use crate::telemetry::redact_query;
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
    // The scheme of the request this service received: plain HTTP from the
    // gateway, unless the URI says otherwise.
    let scheme = request.uri().scheme_str().unwrap_or("http").to_string();
    let query = request
        .uri()
        .query()
        .filter(|q| !q.is_empty())
        .map(redact_query);
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
        url.scheme = %scheme,
        url.path = %path,
        url.query = tracing::field::Empty,
        http.route = tracing::field::Empty,
        request_id = %request_id,
        http.response.status_code = tracing::field::Empty,
        error.type = tracing::field::Empty,
    );
    if let Some(ref r) = route {
        span.record("http.route", r.as_str());
    }
    if let Some(ref q) = query {
        span.record("url.query", q.as_str());
    }

    async move {
        let response = next.run(request).await;
        let status = response.status().as_u16();
        let duration_secs = start.elapsed().as_secs_f64();
        let duration_ms = duration_secs * 1000.0;
        let route_label = route.as_deref().unwrap_or("unmatched");

        metrics::record_request(route_label, &method, status, duration_secs);

        tracing::Span::current().record("http.response.status_code", status);
        let (trace_id, span_id) = trace_ids();

        if status >= 500 {
            let error_type = if status == 503 { "network" } else { "internal" };
            tracing::Span::current().record("error.type", error_type);
            tracing::Span::current().set_status(Status::error(""));
            tracing::error!(
                trace_id = trace_id.as_deref(),
                span_id = span_id.as_deref(),
                route = %route_label,
                method = %method,
                status = status,
                duration_ms = duration_ms,
                request_id = %request_id,
                error_type = error_type,
                error_message = "request failed",
                "request completed"
            );
        } else {
            tracing::info!(
                trace_id = trace_id.as_deref(),
                span_id = span_id.as_deref(),
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

/// The current span's `trace_id` and `span_id`, when it's in a trace
/// (docs/reference/telemetry.md#common-fields).
fn trace_ids() -> (Option<String>, Option<String>) {
    let cx = tracing::Span::current().context();
    let sc = cx.span().span_context().clone();
    if !sc.is_valid() {
        return (None, None);
    }
    (
        Some(sc.trace_id().to_string()),
        Some(sc.span_id().to_string()),
    )
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
        let error_type = if status == 503 { "network" } else { "internal" };
        tracing::error!(
            route = %route.as_deref().unwrap_or("unmatched"),
            method = "GET",
            status = status,
            request_id = %request_id,
            error_type = error_type,
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
    headers: axum::http::HeaderMap,
) -> Result<Response, AppError> {
    let request_id = request_id_from_headers(&headers);
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

/// A feed read: the body stored with the head for one reader, as it was
/// expanded at apply. **204** when nothing newer came within the wait; **404**
/// before the first apply.
async fn feed(
    state: AppState,
    after: Option<i64>,
    headers: &axum::http::HeaderMap,
    reader: Reader,
) -> Result<Response, AppError> {
    let request_id = request_id_from_headers(headers);
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
    feed(state, query.after, &headers, Reader::Gateway).await
}

async fn identity_feed(
    State(state): State<AppState>,
    Query(query): Query<FeedQuery>,
    headers: axum::http::HeaderMap,
) -> Result<Response, AppError> {
    feed(state, query.after, &headers, Reader::Identity).await
}

#[cfg(test)]
mod span_tests {
    use super::http_observability;
    use axum::body::Body;
    use axum::http::{Request, StatusCode};
    use axum::middleware;
    use axum::routing::get;
    use axum::Router;
    use opentelemetry::trace::{Status, TracerProvider};
    use opentelemetry_sdk::trace::{InMemorySpanExporter, SdkTracerProvider, SpanData};
    use std::collections::HashMap;
    use tower::ServiceExt;
    use tracing_subscriber::layer::SubscriberExt;

    /// The HTTP server span for one request through `http_observability`.
    async fn serve(uri: &str) -> SpanData {
        let exporter = InMemorySpanExporter::default();
        let provider = SdkTracerProvider::builder()
            .with_simple_exporter(exporter.clone())
            .build();
        let subscriber = tracing_subscriber::registry()
            .with(tracing_opentelemetry::layer().with_tracer(provider.tracer("test")));
        let _guard = tracing::subscriber::set_default(subscriber);

        let app = Router::new()
            .route("/ok", get(|| async { StatusCode::OK }))
            .route("/missing", get(|| async { StatusCode::NOT_FOUND }))
            .route(
                "/broken",
                get(|| async { StatusCode::INTERNAL_SERVER_ERROR }),
            )
            .route_layer(middleware::from_fn(http_observability));
        app.oneshot(Request::get(uri).body(Body::empty()).unwrap())
            .await
            .unwrap();

        provider.force_flush().unwrap();
        let mut spans = exporter.get_finished_spans().unwrap();
        assert_eq!(spans.len(), 1);
        spans.remove(0)
    }

    fn attrs(span: &SpanData) -> HashMap<String, String> {
        span.attributes
            .iter()
            .map(|kv| (kv.key.to_string(), kv.value.to_string()))
            .collect()
    }

    #[tokio::test]
    async fn url_query_is_redacted() {
        let a = attrs(&serve("/ok?sig=abc&page=2&X-Goog-Signature=g").await);
        assert_eq!(
            a["url.query"],
            "sig=REDACTED&page=2&X-Goog-Signature=REDACTED"
        );
        assert_eq!(a["url.scheme"], "http");
        assert_eq!(a["url.path"], "/ok");
        assert_eq!(a["http.route"], "/ok");
    }

    #[tokio::test]
    async fn no_query_no_url_query() {
        let a = attrs(&serve("/ok").await);
        assert!(!a.contains_key("url.query"), "{a:?}");
    }

    /// The request line, inside a trace: its ids, and the error fields on a 5xx only.
    async fn request_line(uri: &str) -> (serde_json::Value, SpanData) {
        let buf = crate::telemetry::tests::Buffer::default();
        let exporter = InMemorySpanExporter::default();
        let provider = SdkTracerProvider::builder()
            .with_simple_exporter(exporter.clone())
            .build();
        let subscriber = tracing_subscriber::registry()
            .with(crate::telemetry::json_layer(buf.clone()))
            .with(tracing_opentelemetry::layer().with_tracer(provider.tracer("test")));
        let _guard = tracing::subscriber::set_default(subscriber);
        let app = Router::new()
            .route("/ok", get(|| async { StatusCode::OK }))
            .route(
                "/broken",
                get(|| async { StatusCode::INTERNAL_SERVER_ERROR }),
            )
            .route_layer(middleware::from_fn(http_observability));
        app.oneshot(Request::get(uri).body(Body::empty()).unwrap())
            .await
            .unwrap();
        provider.force_flush().unwrap();
        let mut spans = exporter.get_finished_spans().unwrap();
        let out = String::from_utf8(buf.0.lock().unwrap().clone()).unwrap();
        let lines: Vec<&str> = out.lines().collect();
        assert_eq!(lines.len(), 1, "{out}");
        (serde_json::from_str(lines[0]).unwrap(), spans.remove(0))
    }

    #[tokio::test]
    async fn request_line_fields() {
        let (line, span) = request_line("/ok").await;
        assert_eq!(line["level"], "info");
        assert_eq!(line["message"], "request completed");
        assert_eq!(line["route"], "/ok");
        assert_eq!(line["status"], 200);
        assert_eq!(line["trace_id"], span.span_context.trace_id().to_string());
        assert_eq!(line["span_id"], span.span_context.span_id().to_string());
        assert!(line.get("error_type").is_none(), "{line}");

        let (line, _) = request_line("/broken").await;
        assert_eq!(line["level"], "error");
        assert_eq!(line["status"], 500);
        assert_eq!(line["error_type"], "internal");
    }

    #[tokio::test]
    async fn error_type_on_5xx_only() {
        let span = serve("/broken").await;
        assert_eq!(attrs(&span)["error.type"], "internal");
        assert_eq!(span.status, Status::error(""));

        let span = serve("/missing").await;
        assert!(!attrs(&span).contains_key("error.type"));
        assert_eq!(span.status, Status::Unset);
    }
}
