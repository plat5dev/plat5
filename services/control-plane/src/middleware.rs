//! HTTP middleware for request IDs, OpenTelemetry tracing, request logging, and
//! admin bearer auth.

use std::time::Instant;

use axum::extract::{MatchedPath, Request, State};
use axum::http::{header, HeaderValue};
use axum::middleware::Next;
use axum::response::Response;
use opentelemetry::global;
use opentelemetry::propagation::Extractor;
use opentelemetry::trace::{Status, TraceContextExt};
use subtle::ConstantTimeEq;
use tracing::Instrument;
use tracing_opentelemetry::OpenTelemetrySpanExt;
use uuid::Uuid;

use crate::error::AppError;
use crate::metrics;
use crate::telemetry::redact_query;
use crate::AppState;

#[derive(Clone)]
pub(crate) struct RequestId(pub(crate) String);

struct HeaderExtractor<'a>(&'a axum::http::HeaderMap);

impl Extractor for HeaderExtractor<'_> {
    fn get(&self, key: &str) -> Option<&str> {
        self.0.get(key).and_then(|v| v.to_str().ok())
    }

    fn keys(&self) -> Vec<&str> {
        self.0.keys().map(|k| k.as_str()).collect()
    }
}

pub(crate) async fn http_observability(mut request: Request, next: Next) -> Response {
    let start = Instant::now();
    let method = request.method().as_str().to_string();
    let path = request.uri().path().to_string();
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
pub(crate) async fn feed_observability(mut request: Request, next: Next) -> Response {
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

pub(crate) async fn admin_auth_middleware(
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

pub(crate) fn request_id_from_headers(headers: &axum::http::HeaderMap) -> String {
    headers
        .get("x-request-id")
        .and_then(|v| v.to_str().ok())
        .filter(|s| !s.is_empty())
        .map(|s| s.to_string())
        .unwrap_or_else(|| Uuid::new_v4().to_string())
}

pub(crate) fn with_request_id(request_id: String) -> [(header::HeaderName, HeaderValue); 1] {
    let val =
        HeaderValue::from_str(&request_id).unwrap_or_else(|_| HeaderValue::from_static("invalid"));
    [(header::HeaderName::from_static("x-request-id"), val)]
}
