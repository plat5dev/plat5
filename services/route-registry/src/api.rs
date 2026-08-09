use std::collections::HashMap;
use std::time::Instant;

use crate::route_config::{Config, ServiceConfig};
use axum::extract::{MatchedPath, Path, Query, Request, State};
use axum::http::{header, HeaderValue, StatusCode};
use axum::middleware::{self, Next};
use axum::response::{IntoResponse, Response};
use axum::routing::get;
use axum::{Json, Router};
use serde::{Deserialize, Serialize};
use serde_json::json;
use tracing::Instrument;
use uuid::Uuid;

use crate::error::AppError;
use crate::metrics;
use crate::AppState;

pub fn public_router(state: AppState) -> Router {
    let authed = Router::new()
        .route("/v1/services", get(list_services))
        .route(
            "/v1/services/{name}",
            get(get_service).put(put_service).delete(delete_service),
        )
        .route("/v1/apply", axum::routing::post(apply_routes))
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
        .unwrap_or(false)
        || request
            .headers()
            .get("x-plat5-admin-token")
            .and_then(|v| v.to_str().ok())
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
    match state.store.ping().await {
        Ok(()) => (StatusCode::OK, Json(json!({ "status": "healthy" }))).into_response(),
        Err(_) => (
            StatusCode::SERVICE_UNAVAILABLE,
            Json(json!({ "status": "unhealthy" })),
        )
            .into_response(),
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
    let services = state.store.list().await.map_err(|e| {
        tracing::error!(error = %e, "list failed");
        AppError::service_unavailable(request_id.clone())
    })?;
    Ok((
        with_request_id(request_id),
        Json(ServiceListResponse { services }),
    ))
}

async fn get_service(
    State(state): State<AppState>,
    Path(name): Path<String>,
    headers: axum::http::HeaderMap,
) -> Result<impl IntoResponse, AppError> {
    let request_id = request_id_from_headers(&headers);
    let cfg = state.store.get(&name).await.map_err(|e| {
        tracing::error!(error = %e, "get failed");
        AppError::service_unavailable(request_id.clone())
    })?;
    match cfg {
        Some(c) => Ok((with_request_id(request_id), Json(c))),
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
    upsert_one(&state, &name, body, &request_id).await?;
    let cfg = state.store.get(&name).await.map_err(|e| {
        tracing::error!(error = %e, "get after put failed");
        AppError::internal(request_id.clone())
    })?;
    Ok((
        StatusCode::OK,
        with_request_id(request_id),
        Json(cfg.expect("just written")),
    ))
}

#[derive(Deserialize)]
struct DeleteQuery {
    #[serde(default)]
    force: bool,
}

async fn delete_service(
    State(state): State<AppState>,
    Path(name): Path<String>,
    Query(query): Query<DeleteQuery>,
    headers: axum::http::HeaderMap,
) -> Result<impl IntoResponse, AppError> {
    let request_id = request_id_from_headers(&headers);

    if state.platform_services.iter().any(|s| s == &name) && !query.force {
        return Err(AppError::forbidden(
            request_id,
            format!("refusing to delete platform service '{name}' without ?force=true"),
        ));
    }

    let deleted = state.store.delete(&name).await.map_err(|e| {
        tracing::error!(error = %e, "delete failed");
        AppError::service_unavailable(request_id.clone())
    })?;
    if !deleted {
        return Err(AppError::not_found(request_id, "service", &name));
    }
    Ok((StatusCode::NO_CONTENT, with_request_id(request_id)))
}

#[derive(Serialize)]
struct ApplyResult {
    service: String,
    status: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    error: Option<String>,
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

    // Best-effort per service after full validation. Mid-batch write failure
    // leaves earlier upserts live; response reports per-service status.
    let mut results = Vec::new();
    let mut failed = false;
    for (name, service) in config.services {
        if failed {
            results.push(ApplyResult {
                service: name,
                status: "skipped".into(),
                error: None,
            });
            continue;
        }
        match upsert_one(&state, &name, service, &request_id).await {
            Ok(()) => results.push(ApplyResult {
                service: name,
                status: "upserted".into(),
                error: None,
            }),
            Err(e) => {
                tracing::error!(
                    service = %name,
                    error = %e.message,
                    "apply upsert failed; remaining services skipped"
                );
                results.push(ApplyResult {
                    service: name,
                    status: "failed".into(),
                    error: Some(e.message),
                });
                failed = true;
            }
        }
    }

    let status = if failed {
        StatusCode::SERVICE_UNAVAILABLE
    } else {
        StatusCode::OK
    };

    Ok((
        status,
        with_request_id(request_id),
        Json(ApplyResponse { results }),
    ))
}

async fn upsert_one(
    state: &AppState,
    name: &str,
    service: ServiceConfig,
    request_id: &str,
) -> Result<(), AppError> {
    if name.is_empty() {
        return Err(AppError::invalid_request(
            request_id.to_string(),
            "service name must not be empty",
        ));
    }

    let prepared = service
        .prepare_for_registry(name)
        .map_err(|e| AppError::validation(request_id.to_string(), e.to_string()))?;

    state.store.put(name, &prepared).await.map_err(|e| {
        tracing::error!(error = %e, service = %name, "put failed");
        AppError::service_unavailable(request_id.to_string())
    })?;
    Ok(())
}
