use std::time::{Duration, Instant};

use axum::extract::rejection::{PathRejection, QueryRejection};
use axum::extract::{Extension, MatchedPath, Path, Query, Request, State};
use axum::http::{header, HeaderValue, StatusCode};
use axum::middleware::{self, Next};
use axum::response::{IntoResponse, Response};
use axum::routing::{get, post};
use axum::{Json, Router};
use opentelemetry::global;
use opentelemetry::propagation::Extractor;
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

#[derive(Clone)]
struct RequestId(String);

struct HeaderExtractor<'a>(&'a axum::http::HeaderMap);

impl Extractor for HeaderExtractor<'_> {
    fn get(&self, key: &str) -> Option<&str> {
        self.0.get(key).and_then(|v| v.to_str().ok())
    }

    fn keys(&self) -> Vec<&str> {
        self.0.keys().map(|k| k.as_str()).collect()
    }
}

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

async fn http_observability(mut request: Request, next: Next) -> Response {
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
    request
        .extensions_mut()
        .insert(RequestId(request_id.clone()));
    let parent_cx = global::get_text_map_propagator(|propagator| {
        propagator.extract(&HeaderExtractor(request.headers()))
    });

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
    let _ = span.set_parent(parent_cx);
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
async fn feed_observability(mut request: Request, next: Next) -> Response {
    let route = request
        .extensions()
        .get::<MatchedPath>()
        .map(|m| m.as_str().to_string());
    let request_id = request_id_from_headers(request.headers());
    request
        .extensions_mut()
        .insert(RequestId(request_id.clone()));
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
    let request_id = request
        .extensions()
        .get::<RequestId>()
        .map(|id| id.0.clone())
        .unwrap_or_else(|| request_id_from_headers(request.headers()));
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

#[cfg(test)]
mod tests {
    use std::sync::{Arc, Mutex};

    use axum::body::Body;
    use axum::http::Request;
    use opentelemetry::trace::TracerProvider;
    use opentelemetry_sdk::propagation::TraceContextPropagator;
    use opentelemetry_sdk::trace as sdktrace;
    use tokio::sync::watch;
    use tower::ServiceExt;
    use tracing_opentelemetry::OpenTelemetryLayer;
    use tracing_subscriber::fmt::MakeWriter;
    use tracing_subscriber::layer::SubscriberExt;

    use super::*;
    use crate::counts::NoCounts;
    use crate::feed::Feed;
    use crate::store::Store;

    const TOKEN: &str = "test-admin-token";

    #[derive(Clone, Default)]
    struct LogBuf(Arc<Mutex<Vec<u8>>>);

    impl std::io::Write for LogBuf {
        fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
            self.0.lock().unwrap().extend_from_slice(buf);
            Ok(buf.len())
        }
        fn flush(&mut self) -> std::io::Result<()> {
            Ok(())
        }
    }

    impl<'a> MakeWriter<'a> for LogBuf {
        type Writer = LogBuf;
        fn make_writer(&'a self) -> Self::Writer {
            self.clone()
        }
    }

    fn test_state(head: i64) -> (AppState, watch::Sender<bool>) {
        let (tx, rx) = watch::channel(false);
        let state = AppState {
            store: Store::lazy_for_test(),
            feed: Feed::new_for_test(head, rx),
            counts: Arc::new(NoCounts),
            admin_token: TOKEN.into(),
        };
        (state, tx)
    }

    #[tokio::test]
    async fn single_request_id_matches_header_body_and_log() {
        let buf = LogBuf::default();
        let subscriber =
            tracing_subscriber::registry().with(crate::telemetry::json_layer(buf.clone()));
        let _guard = tracing::subscriber::set_default(subscriber);

        let (state, _tx) = test_state(0);
        let router = public_router(state);

        // Unauthorized request without X-Request-ID mints one ID shared everywhere.
        let resp = router
            .clone()
            .oneshot(Request::get("/config").body(Body::empty()).unwrap())
            .await
            .unwrap();
        assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
        let header_id = resp
            .headers()
            .get("x-request-id")
            .unwrap()
            .to_str()
            .unwrap()
            .to_string();
        let bytes = axum::body::to_bytes(resp.into_body(), 1 << 16)
            .await
            .unwrap();
        let body: Value = serde_json::from_slice(&bytes).unwrap();
        assert_eq!(body["error"]["request_id"], json!(header_id));

        let log_text = String::from_utf8(buf.0.lock().unwrap().clone()).unwrap();
        let line: Value = serde_json::from_str(log_text.lines().last().unwrap()).unwrap();
        assert_eq!(line["request_id"], json!(header_id));

        // 200 response without X-Request-ID also shares one minted ID across header and log.
        let resp = router
            .oneshot(Request::get("/schema").body(Body::empty()).unwrap())
            .await
            .unwrap();
        assert_eq!(resp.status(), StatusCode::OK);
        let schema_header_id = resp
            .headers()
            .get("x-request-id")
            .unwrap()
            .to_str()
            .unwrap()
            .to_string();
        let log_text = String::from_utf8(buf.0.lock().unwrap().clone()).unwrap();
        let line: Value = serde_json::from_str(log_text.lines().last().unwrap()).unwrap();
        assert_eq!(line["request_id"], json!(schema_header_id));
    }

    #[tokio::test]
    async fn malformed_path_and_query_return_plat5_error_envelope() {
        let (state, _tx) = test_state(0);
        let router = public_router(state);

        // Malformed path param on GET /revisions/{rev}
        let resp = router
            .clone()
            .oneshot(
                Request::get("/revisions/not-a-number")
                    .header("authorization", format!("Bearer {TOKEN}"))
                    .header("x-request-id", "req-path-1")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(resp.status(), StatusCode::BAD_REQUEST);
        assert_eq!(
            resp.headers()
                .get("x-request-id")
                .unwrap()
                .to_str()
                .unwrap(),
            "req-path-1"
        );
        let bytes = axum::body::to_bytes(resp.into_body(), 1 << 16)
            .await
            .unwrap();
        let body: Value = serde_json::from_slice(&bytes).unwrap();
        assert_eq!(body["error"]["code"], json!("INVALID_REQUEST"));
        assert_eq!(body["error"]["request_id"], json!("req-path-1"));

        // Malformed query param on POST /apply
        let resp = router
            .clone()
            .oneshot(
                Request::post("/apply?dry_run=notabool")
                    .header("authorization", format!("Bearer {TOKEN}"))
                    .header("x-request-id", "req-query-1")
                    .body(Body::from("{}"))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(resp.status(), StatusCode::BAD_REQUEST);
        assert_eq!(
            resp.headers()
                .get("x-request-id")
                .unwrap()
                .to_str()
                .unwrap(),
            "req-query-1"
        );
        let bytes = axum::body::to_bytes(resp.into_body(), 1 << 16)
            .await
            .unwrap();
        let body: Value = serde_json::from_slice(&bytes).unwrap();
        assert_eq!(body["error"]["code"], json!("INVALID_REQUEST"));
        assert_eq!(body["error"]["request_id"], json!("req-query-1"));
    }

    #[tokio::test]
    async fn incoming_traceparent_sets_server_span_trace_id() {
        global::set_text_map_propagator(TraceContextPropagator::new());
        let provider = sdktrace::SdkTracerProvider::builder().build();
        let buf = LogBuf::default();
        let subscriber = tracing_subscriber::registry()
            .with(crate::telemetry::json_layer(buf.clone()))
            .with(OpenTelemetryLayer::new(provider.tracer("test")));
        let _guard = tracing::subscriber::set_default(subscriber);

        let (state, _tx) = test_state(0);
        let router = public_router(state);

        let trace_id = "0af7651916cd43dd8448eb211c80319c";
        let resp = router
            .oneshot(
                Request::get("/schema")
                    .header("traceparent", format!("00-{trace_id}-b7ad6b7169203331-01"))
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(resp.status(), StatusCode::OK);

        let log_text = String::from_utf8(buf.0.lock().unwrap().clone()).unwrap();
        let line: Value = serde_json::from_str(log_text.lines().last().unwrap()).unwrap();
        assert_eq!(line["trace_id"], json!(trace_id));
    }

    #[tokio::test]
    async fn long_poll_returns_204_immediately_on_shutdown() {
        let (state, shutdown_tx) = test_state(5);
        let router = internal_router(state);

        let poll = tokio::spawn(async move {
            router
                .oneshot(
                    Request::get("/internal/identity?after=5")
                        .body(Body::empty())
                        .unwrap(),
                )
                .await
                .unwrap()
        });
        tokio::time::sleep(Duration::from_millis(20)).await;
        shutdown_tx.send(true).unwrap();

        let resp = tokio::time::timeout(Duration::from_secs(1), poll)
            .await
            .expect("long-poll should finish promptly on shutdown")
            .unwrap();
        assert_eq!(resp.status(), StatusCode::NO_CONTENT);
    }
}
