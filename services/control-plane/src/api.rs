use std::collections::HashMap;
use std::time::{Duration, Instant};

use crate::route_config::{
    find_route_conflicts, validate_shared_rate_limits, Config, ServiceConfig,
};
use axum::extract::{MatchedPath, Path, Query, Request, State};
use axum::http::{header, HeaderValue, StatusCode};
use axum::middleware::{self, Next};
use axum::response::{IntoResponse, Response};
use axum::routing::{get, post};
use axum::{Json, Router};
use opentelemetry::trace::Status;
use serde::de::DeserializeOwned;
use serde::{Deserialize, Serialize};
use serde_json::json;
use tracing::Instrument;
use tracing_opentelemetry::OpenTelemetrySpanExt;
use uuid::Uuid;

use crate::documents::{Cors, Document, Roles};
use crate::error::AppError;
use crate::metrics;
use crate::pg_store::{CommitResult, DocumentRevision, Revision};
use crate::AppState;

/// How long an internal read with `after` waits for a newer revision.
const LONG_POLL_WAIT: Duration = Duration::from_secs(25);

pub fn public_router(state: AppState) -> Router {
    let authed = Router::new()
        .route("/services", get(list_services))
        .route(
            "/services/{name}",
            get(get_service).put(put_service).delete(delete_service),
        )
        .route("/services/{name}/revisions", get(list_revisions))
        .route("/services/{name}/revisions/{rev}", get(get_revision))
        .route(
            "/services/{name}/revisions/{rev}/restore",
            post(restore_revision),
        )
        .route("/apply", post(apply_routes));
    let authed = document_routes::<Cors>(document_routes::<Roles>(authed)).layer(
        middleware::from_fn_with_state(state.clone(), admin_auth_middleware),
    );

    Router::new()
        .merge(authed)
        .route_layer(middleware::from_fn(http_observability))
        .with_state(state)
}

/// `/roles`, `/cors`: the current document, its history, and restore.
fn document_routes<D: Document>(router: Router<AppState>) -> Router<AppState> {
    let base = format!("/{}", D::KIND);
    router
        .route(&base, get(get_document::<D>).put(put_document::<D>))
        .route(
            &format!("{base}/revisions"),
            get(list_document_revisions::<D>),
        )
        .route(
            &format!("{base}/revisions/{{rev}}"),
            get(get_document_revision::<D>),
        )
        .route(
            &format!("{base}/revisions/{{rev}}/restore"),
            post(restore_document::<D>),
        )
}

pub fn internal_router(state: AppState) -> Router {
    // What the runtime reads. Behind INTERNAL_AUTH_TOKEN when it is set.
    let feeds = Router::new()
        .route("/internal/gateway", get(gateway_feed))
        .route("/internal/roles", get(roles_feed))
        .layer(middleware::from_fn_with_state(
            state.clone(),
            internal_auth_middleware,
        ))
        .route_layer(middleware::from_fn(http_observability));

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
        .map(|t| t == state.admin_token)
        .unwrap_or(false);

    if !authorized {
        return Err(AppError::unauthorized(request_id));
    }

    Ok(next.run(request).await)
}

async fn internal_auth_middleware(
    State(state): State<AppState>,
    request: Request,
    next: Next,
) -> Result<Response, AppError> {
    if let Some(expected) = state.internal_token.as_deref() {
        let got = request
            .headers()
            .get("x-plat5-internal-token")
            .and_then(|v| v.to_str().ok());
        if got != Some(expected) {
            return Err(AppError::unauthorized_internal(request_id_from_headers(
                request.headers(),
            )));
        }
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
    if state.pg.ping().await.is_ok() {
        (StatusCode::OK, Json(json!({ "status": "healthy" }))).into_response()
    } else {
        (
            StatusCode::SERVICE_UNAVAILABLE,
            Json(json!({ "status": "unhealthy" })),
        )
            .into_response()
    }
}

#[derive(Serialize)]
struct ServiceListResponse {
    services: HashMap<String, ServiceConfig>,
}

async fn list_services(
    State(state): State<AppState>,
    headers: axum::http::HeaderMap,
) -> Result<impl IntoResponse, AppError> {
    let request_id = request_id_from_headers(&headers);
    let services = state.pg.list_current().await.map_err(|e| {
        tracing::error!(error = %e, "list failed");
        AppError::service_unavailable(request_id.clone())
    })?;
    Ok((
        with_request_id(request_id),
        Json(ServiceListResponse { services }),
    ))
}

#[derive(Serialize)]
struct ServiceResponse {
    name: String,
    revision: i64,
    #[serde(flatten)]
    config: ServiceConfig,
}

async fn get_service(
    State(state): State<AppState>,
    Path(name): Path<String>,
    headers: axum::http::HeaderMap,
) -> Result<impl IntoResponse, AppError> {
    let request_id = request_id_from_headers(&headers);
    let current = state.pg.get_current(&name).await.map_err(|e| {
        tracing::error!(error = %e, "get failed");
        AppError::service_unavailable(request_id.clone())
    })?;
    match current {
        Some((revision, config)) => Ok((
            with_request_id(request_id),
            Json(ServiceResponse {
                name,
                revision,
                config,
            }),
        )),
        None => Err(AppError::not_found(request_id, "service", &name)),
    }
}

async fn put_service(
    State(state): State<AppState>,
    Path(name): Path<String>,
    headers: axum::http::HeaderMap,
    Json(body): Json<ServiceConfig>,
) -> Result<impl IntoResponse, AppError> {
    let request_id = request_id_from_headers(&headers);
    let prepared = check_upserts(&state, vec![(name, body)], &request_id).await?;
    let commits = commit(&state, prepared, &request_id).await?;
    let commit = commits.into_iter().next().expect("one commit");
    let config = commit.config.expect("put is not a delete");
    Ok((
        StatusCode::OK,
        with_request_id(request_id),
        Json(ServiceResponse {
            name: commit.service,
            revision: commit.revision,
            config,
        }),
    ))
}

async fn delete_service(
    State(state): State<AppState>,
    Path(name): Path<String>,
    headers: axum::http::HeaderMap,
) -> Result<impl IntoResponse, AppError> {
    let request_id = request_id_from_headers(&headers);
    if state
        .pg
        .get_current(&name)
        .await
        .map_err(|e| {
            tracing::error!(error = %e, "get before delete failed");
            AppError::service_unavailable(request_id.clone())
        })?
        .is_none()
    {
        return Err(AppError::not_found(request_id, "service", &name));
    }

    commit(&state, vec![(name, None)], &request_id).await?;
    Ok((StatusCode::NO_CONTENT, with_request_id(request_id)))
}

#[derive(Serialize)]
struct ApplyResult {
    service: String,
    status: String,
    revision: i64,
}

#[derive(Serialize)]
struct ApplyResponse {
    results: Vec<ApplyResult>,
}

async fn apply_routes(
    State(state): State<AppState>,
    headers: axum::http::HeaderMap,
    body: axum::body::Bytes,
) -> Result<impl IntoResponse, AppError> {
    let request_id = request_id_from_headers(&headers);
    let config: Config = parse_body(&headers, &body, &request_id, "route config")?;

    if config.services.is_empty() {
        return Err(AppError::invalid_request(
            request_id,
            "services map must not be empty",
        ));
    }

    config
        .validate()
        .map_err(|e| AppError::validation(request_id.clone(), e.to_string()))?;

    let prepared =
        check_upserts(&state, config.services.into_iter().collect(), &request_id).await?;

    let commits = commit(&state, prepared, &request_id).await?;
    let results = commits
        .into_iter()
        .map(|c| ApplyResult {
            service: c.service,
            status: "upserted".into(),
            revision: c.revision,
        })
        .collect();

    Ok((
        StatusCode::OK,
        with_request_id(request_id),
        Json(ApplyResponse { results }),
    ))
}

#[derive(Serialize)]
struct RevisionResponse {
    service: String,
    revision: i64,
    config: Option<ServiceConfig>,
    request_id: String,
    created_at: String,
}

#[derive(Serialize)]
struct RevisionListResponse {
    revisions: Vec<RevisionResponse>,
}

async fn list_revisions(
    State(state): State<AppState>,
    Path(name): Path<String>,
    headers: axum::http::HeaderMap,
) -> Result<impl IntoResponse, AppError> {
    let request_id = request_id_from_headers(&headers);
    let revisions = state.pg.list_revisions(&name).await.map_err(|e| {
        tracing::error!(error = %e, "list revisions failed");
        AppError::service_unavailable(request_id.clone())
    })?;
    match revisions {
        Some(list) => Ok((
            with_request_id(request_id),
            Json(RevisionListResponse {
                revisions: list.into_iter().map(to_revision_response).collect(),
            }),
        )),
        None => Err(AppError::not_found(request_id, "service", &name)),
    }
}

async fn get_revision(
    State(state): State<AppState>,
    Path((name, rev)): Path<(String, i64)>,
    headers: axum::http::HeaderMap,
) -> Result<impl IntoResponse, AppError> {
    let request_id = request_id_from_headers(&headers);
    let revision = state.pg.get_revision(&name, rev).await.map_err(|e| {
        tracing::error!(error = %e, "get revision failed");
        AppError::service_unavailable(request_id.clone())
    })?;
    match revision {
        Some(r) => Ok((with_request_id(request_id), Json(to_revision_response(r)))),
        None => Err(AppError::not_found(
            request_id,
            "revision",
            &format!("{name}#{rev}"),
        )),
    }
}

async fn restore_revision(
    State(state): State<AppState>,
    Path((name, rev)): Path<(String, i64)>,
    headers: axum::http::HeaderMap,
) -> Result<impl IntoResponse, AppError> {
    let request_id = request_id_from_headers(&headers);
    let revision = state.pg.get_revision(&name, rev).await.map_err(|e| {
        tracing::error!(error = %e, "get revision for restore failed");
        AppError::service_unavailable(request_id.clone())
    })?;
    let Some(revision) = revision else {
        return Err(AppError::not_found(
            request_id,
            "revision",
            &format!("{name}#{rev}"),
        ));
    };
    let Some(config) = revision.config else {
        return Err(AppError::validation(
            request_id,
            "cannot restore a delete revision".into(),
        ));
    };

    // A revision is restored as if it were put now: current url form, path
    // form, shared rate limits and route conflicts all apply.
    let prepared = check_upserts(&state, vec![(name, config)], &request_id).await?;
    let commits = commit(&state, prepared, &request_id).await?;
    let commit = commits.into_iter().next().expect("one commit");
    let config = commit.config.expect("restore is not a delete");
    Ok((
        StatusCode::OK,
        with_request_id(request_id),
        Json(ServiceResponse {
            name: commit.service,
            revision: commit.revision,
            config,
        }),
    ))
}

fn to_revision_response(r: Revision) -> RevisionResponse {
    RevisionResponse {
        service: r.service_name,
        revision: r.revision,
        config: r.config,
        request_id: r.request_id,
        created_at: r.created_at.to_rfc3339(),
    }
}

async fn ensure_shared_policies(
    state: &AppState,
    overlay: &[(String, Option<ServiceConfig>)],
    request_id: &str,
) -> Result<(), AppError> {
    let mut all = state.pg.list_current().await.map_err(|e| {
        tracing::error!(error = %e, "list current for shared rate_limits failed");
        AppError::service_unavailable(request_id.to_string())
    })?;
    for (name, cfg) in overlay {
        match cfg {
            Some(c) => {
                all.insert(name.clone(), c.clone());
            }
            None => {
                all.remove(name);
            }
        }
    }
    validate_shared_rate_limits(&all)
        .map_err(|e| AppError::validation(request_id.to_string(), e.to_string()))
}

async fn ensure_no_route_conflicts(
    state: &AppState,
    items: &[(String, Option<ServiceConfig>)],
    request_id: &str,
) -> Result<(), AppError> {
    let existing = state.pg.list_current().await.map_err(|e| {
        tracing::error!(error = %e, "list current for route conflicts failed");
        AppError::service_unavailable(request_id.to_string())
    })?;
    let incoming: Vec<(String, ServiceConfig)> = items
        .iter()
        .filter_map(|(n, c)| c.clone().map(|c| (n.clone(), c)))
        .collect();
    let conflicts = find_route_conflicts(&existing, &incoming);
    if conflicts.is_empty() {
        return Ok(());
    }
    Err(AppError::route_conflict(
        request_id.to_string(),
        format!(
            "Route already owned by another service: {}.",
            conflicts.join("; ")
        ),
    ))
}

/// A write body in YAML (by content type) or JSON, falling back to YAML.
fn parse_body<T: DeserializeOwned>(
    headers: &axum::http::HeaderMap,
    body: &[u8],
    request_id: &str,
    what: &str,
) -> Result<T, AppError> {
    let content_type = headers
        .get(header::CONTENT_TYPE)
        .and_then(|v| v.to_str().ok())
        .unwrap_or("application/json");
    if content_type.contains("yaml") || content_type.contains("yml") {
        return serde_yaml::from_slice(body)
            .map_err(|e| parse_error(request_id.to_string(), what, &e));
    }
    match serde_json::from_slice(body) {
        Ok(c) => Ok(c),
        Err(json_err) => serde_yaml::from_slice(body).map_err(|yaml_err| {
            parse_error(
                request_id.to_string(),
                what,
                &format!("invalid JSON ({json_err}) or YAML ({yaml_err})"),
            )
        }),
    }
}

fn parse_error(request_id: String, what: &str, err: &impl std::fmt::Display) -> AppError {
    let msg = err.to_string();
    if msg.contains("unknown field") {
        AppError::validation(request_id, format!("unknown field: {msg}"))
    } else {
        AppError::invalid_request(request_id, format!("invalid {what}: {msg}"))
    }
}

/// The one validation path for every write that upserts services (apply, put,
/// restore): per-service config (url, paths, routes), shared rate limits
/// against current state, and 409 route conflicts with other services.
async fn check_upserts(
    state: &AppState,
    items: Vec<(String, ServiceConfig)>,
    request_id: &str,
) -> Result<Vec<(String, Option<ServiceConfig>)>, AppError> {
    let mut prepared = Vec::with_capacity(items.len());
    for (name, service) in items {
        let cfg = prepare_named(&name, service, request_id)?;
        prepared.push((name, Some(cfg)));
    }
    ensure_shared_policies(state, &prepared, request_id).await?;
    ensure_no_route_conflicts(state, &prepared, request_id).await?;
    Ok(prepared)
}

fn prepare_named(
    name: &str,
    service: ServiceConfig,
    request_id: &str,
) -> Result<ServiceConfig, AppError> {
    if name.is_empty() {
        return Err(AppError::invalid_request(
            request_id.to_string(),
            "service name must not be empty",
        ));
    }
    service
        .prepare_for_registry(name)
        .map_err(|e| AppError::validation(request_id.to_string(), e.to_string()))
}

async fn commit(
    state: &AppState,
    items: Vec<(String, Option<ServiceConfig>)>,
    request_id: &str,
) -> Result<Vec<CommitResult>, AppError> {
    let (commits, head) = state
        .pg
        .commit_batch(&items, request_id)
        .await
        .map_err(|e| {
            tracing::error!(error = %e, "postgres commit failed");
            AppError::service_unavailable(request_id.to_string())
        })?;
    state.feed.advance(head);
    Ok(commits)
}

#[derive(Serialize)]
struct DocumentResponse<D> {
    revision: i64,
    #[serde(flatten)]
    document: D,
}

#[derive(Serialize)]
struct DocumentRevisionResponse {
    kind: &'static str,
    revision: i64,
    config: serde_json::Value,
    request_id: String,
    created_at: String,
}

#[derive(Serialize)]
struct DocumentRevisionListResponse {
    revisions: Vec<DocumentRevisionResponse>,
}

fn to_document_revision_response<D: Document>(r: DocumentRevision) -> DocumentRevisionResponse {
    DocumentRevisionResponse {
        kind: D::KIND,
        revision: r.revision,
        config: r.body,
        request_id: r.request_id,
        created_at: r.created_at.to_rfc3339(),
    }
}

fn pg_unavailable<'a>(
    request_id: &'a str,
    what: &'static str,
) -> impl FnOnce(crate::pg_store::PgError) -> AppError + 'a {
    move |e| {
        tracing::error!(error = %e, "{what} failed");
        AppError::service_unavailable(request_id.to_string())
    }
}

async fn get_document<D: Document>(
    State(state): State<AppState>,
    headers: axum::http::HeaderMap,
) -> Result<impl IntoResponse, AppError> {
    let request_id = request_id_from_headers(&headers);
    let current = state
        .pg
        .get_document(D::KIND)
        .await
        .map_err(pg_unavailable(&request_id, "get document"))?;
    let Some(current) = current else {
        return Err(AppError::not_found(request_id, "document", D::KIND));
    };
    let document: D = serde_json::from_value(current.body).map_err(|e| {
        tracing::error!(error = %e, kind = D::KIND, "stored document does not parse");
        AppError::service_unavailable(request_id.clone())
    })?;
    Ok((
        with_request_id(request_id),
        Json(DocumentResponse {
            revision: current.revision,
            document,
        }),
    ))
}

async fn put_document<D: Document>(
    State(state): State<AppState>,
    headers: axum::http::HeaderMap,
    body: axum::body::Bytes,
) -> Result<impl IntoResponse, AppError> {
    let request_id = request_id_from_headers(&headers);
    let what = format!("{} document", D::KIND);
    let document: D = parse_body(&headers, &body, &request_id, &what)?;
    let revision = commit_document(&state, &document, &request_id).await?;
    Ok((
        StatusCode::OK,
        with_request_id(request_id),
        Json(DocumentResponse { revision, document }),
    ))
}

async fn list_document_revisions<D: Document>(
    State(state): State<AppState>,
    headers: axum::http::HeaderMap,
) -> Result<impl IntoResponse, AppError> {
    let request_id = request_id_from_headers(&headers);
    let revisions = state
        .pg
        .list_document_revisions(D::KIND)
        .await
        .map_err(pg_unavailable(&request_id, "list document revisions"))?;
    match revisions {
        Some(list) => Ok((
            with_request_id(request_id),
            Json(DocumentRevisionListResponse {
                revisions: list
                    .into_iter()
                    .map(to_document_revision_response::<D>)
                    .collect(),
            }),
        )),
        None => Err(AppError::not_found(request_id, "document", D::KIND)),
    }
}

async fn get_document_revision<D: Document>(
    State(state): State<AppState>,
    Path(rev): Path<i64>,
    headers: axum::http::HeaderMap,
) -> Result<impl IntoResponse, AppError> {
    let request_id = request_id_from_headers(&headers);
    let revision = state
        .pg
        .get_document_revision(D::KIND, rev)
        .await
        .map_err(pg_unavailable(&request_id, "get document revision"))?;
    match revision {
        Some(r) => Ok((
            with_request_id(request_id),
            Json(to_document_revision_response::<D>(r)),
        )),
        None => Err(AppError::not_found(
            request_id,
            "revision",
            &format!("{}#{rev}", D::KIND),
        )),
    }
}

async fn restore_document<D: Document>(
    State(state): State<AppState>,
    Path(rev): Path<i64>,
    headers: axum::http::HeaderMap,
) -> Result<impl IntoResponse, AppError> {
    let request_id = request_id_from_headers(&headers);
    let revision = state
        .pg
        .get_document_revision(D::KIND, rev)
        .await
        .map_err(pg_unavailable(
            &request_id,
            "get document revision for restore",
        ))?;
    let Some(revision) = revision else {
        return Err(AppError::not_found(
            request_id,
            "revision",
            &format!("{}#{rev}", D::KIND),
        ));
    };
    // Restored as if written now: today's rules apply.
    let document: D = serde_json::from_value(revision.body).map_err(|e| {
        AppError::validation(
            request_id.clone(),
            format!("revision no longer parses: {e}"),
        )
    })?;
    let revision = commit_document(&state, &document, &request_id).await?;
    Ok((
        StatusCode::OK,
        with_request_id(request_id),
        Json(DocumentResponse { revision, document }),
    ))
}

/// Validates and writes a document. Returns its new revision.
async fn commit_document<D: Document>(
    state: &AppState,
    document: &D,
    request_id: &str,
) -> Result<i64, AppError> {
    document
        .validate()
        .map_err(|e| AppError::validation(request_id.to_string(), format!("{}: {e}", D::KIND)))?;
    let body = serde_json::to_value(document).map_err(|e| {
        tracing::error!(error = %e, "document serialize failed");
        AppError::service_unavailable(request_id.to_string())
    })?;
    let (revision, head) = state
        .pg
        .commit_document(D::KIND, &body, request_id)
        .await
        .map_err(pg_unavailable(request_id, "commit document"))?;
    state.feed.advance(head);
    Ok(revision)
}

#[derive(Deserialize)]
struct FeedQuery {
    after: Option<i64>,
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

#[derive(Serialize)]
struct GatewayFeed {
    revision: i64,
    services: HashMap<String, ServiceConfig>,
    cors: Cors,
}

/// What a gateway runs on: every current service and the CORS document.
async fn gateway_feed(
    State(state): State<AppState>,
    Query(query): Query<FeedQuery>,
    headers: axum::http::HeaderMap,
) -> Result<Response, AppError> {
    let request_id = request_id_from_headers(&headers);
    if !wait_for_newer(&state, query.after).await {
        return Ok(StatusCode::NO_CONTENT.into_response());
    }
    let snapshot = state
        .pg
        .gateway_snapshot()
        .await
        .map_err(pg_unavailable(&request_id, "gateway snapshot"))?;
    let cors = match snapshot.cors {
        Some(body) => serde_json::from_value(body).map_err(|e| {
            tracing::error!(error = %e, "stored cors document does not parse");
            AppError::service_unavailable(request_id.clone())
        })?,
        None => Cors::default(),
    };
    Ok(Json(GatewayFeed {
        revision: snapshot.revision,
        services: snapshot.services,
        cors,
    })
    .into_response())
}

#[derive(Serialize)]
struct RolesFeed {
    revision: i64,
    roles: Roles,
}

/// What identity runs on: the roles document. **404** until one is written.
async fn roles_feed(
    State(state): State<AppState>,
    Query(query): Query<FeedQuery>,
    headers: axum::http::HeaderMap,
) -> Result<Response, AppError> {
    let request_id = request_id_from_headers(&headers);
    if !wait_for_newer(&state, query.after).await {
        return Ok(StatusCode::NO_CONTENT.into_response());
    }
    let (revision, roles) = state
        .pg
        .roles_snapshot()
        .await
        .map_err(pg_unavailable(&request_id, "roles snapshot"))?;
    let Some(roles) = roles else {
        return Err(AppError::not_found(request_id, "document", Roles::KIND));
    };
    let roles: Roles = serde_json::from_value(roles).map_err(|e| {
        tracing::error!(error = %e, "stored roles document does not parse");
        AppError::service_unavailable(request_id.clone())
    })?;
    Ok(Json(RolesFeed { revision, roles }).into_response())
}
