use std::collections::HashMap;
use std::time::Instant;

use crate::route_config::{Config, ServiceConfig};
use axum::extract::{MatchedPath, Path, Request, State};
use axum::http::{header, HeaderValue, StatusCode};
use axum::middleware::{self, Next};
use axum::response::{IntoResponse, Response};
use axum::routing::{get, post};
use axum::{Json, Router};
use serde::Serialize;
use serde_json::json;
use tracing::Instrument;
use uuid::Uuid;

use crate::error::AppError;
use crate::metrics;
use crate::pg_store::{CommitResult, Revision};
use crate::projection;
use crate::AppState;

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
        .route("/apply", post(apply_routes))
        .layer(middleware::from_fn_with_state(
            state.clone(),
            admin_auth_middleware,
        ));

    Router::new()
        .merge(authed)
        .layer(middleware::from_fn(http_observability))
        .with_state(state)
}

pub fn internal_router(state: AppState) -> Router {
    Router::new()
        .route("/health/live", get(health_live))
        .route("/health/ready", get(health_ready))
        .route("/metrics", get(metrics_scrape))
        .with_state(state)
}

async fn http_observability(request: Request, next: Next) -> Response {
    let start = Instant::now();
    let method = request.method().as_str().to_string();
    let route = request
        .extensions()
        .get::<MatchedPath>()
        .map(|m| m.as_str().to_string())
        .unwrap_or_else(|| request.uri().path().to_string());
    let request_id = request_id_from_headers(request.headers());

    let span = tracing::info_span!(
        "route_registry.request",
        http.method = %method,
        http.route = %route,
        request_id = %request_id,
        http.status_code = tracing::field::Empty,
        error.kind = tracing::field::Empty,
    );

    async move {
        let response = next.run(request).await;
        let status = response.status().as_u16();
        let duration_secs = start.elapsed().as_secs_f64();
        let duration_ms = duration_secs * 1000.0;

        metrics::record_request(&route, &method, status, duration_secs);

        tracing::Span::current().record("http.status_code", status);

        if status >= 500 {
            let error_kind = if status == 503 { "network" } else { "internal" };
            tracing::Span::current().record("error.kind", error_kind);
            tracing::error!(
                route = %route,
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
                route = %route,
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
    let pg_ok = state.pg.ping().await.is_ok();
    let etcd_ok = state.etcd.ping().await.is_ok();
    if pg_ok && etcd_ok {
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
    let prepared = prepare_named(&name, body, &request_id)?;
    let commits = commit(&state, vec![(name.clone(), Some(prepared))], &request_id).await?;
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
    let content_type = headers
        .get(header::CONTENT_TYPE)
        .and_then(|v| v.to_str().ok())
        .unwrap_or("application/json");

    let config: Config = if content_type.contains("yaml") || content_type.contains("yml") {
        serde_yaml::from_slice(&body).map_err(|e| {
            AppError::invalid_request(request_id.clone(), format!("invalid YAML: {e}"))
        })?
    } else {
        match serde_json::from_slice(&body) {
            Ok(c) => c,
            Err(_) => serde_yaml::from_slice(&body).map_err(|e| {
                AppError::invalid_request(
                    request_id.clone(),
                    format!("invalid JSON or YAML body: {e}"),
                )
            })?,
        }
    };

    if config.services.is_empty() {
        return Err(AppError::invalid_request(
            request_id,
            "services map must not be empty",
        ));
    }

    config
        .validate()
        .map_err(|e| AppError::validation(request_id.clone(), e.to_string()))?;

    let mut prepared = Vec::new();
    for (name, service) in config.services {
        let cfg = prepare_named(&name, service, &request_id)?;
        prepared.push((name, Some(cfg)));
    }

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

    let commits = commit(&state, vec![(name, Some(config))], &request_id).await?;
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
    let commits = state
        .pg
        .commit_batch(&items, request_id)
        .await
        .map_err(|e| {
            tracing::error!(error = %e, "postgres commit failed");
            AppError::service_unavailable(request_id.to_string())
        })?;
    projection::project_commits(&state.etcd, &commits).await;
    Ok(commits)
}
