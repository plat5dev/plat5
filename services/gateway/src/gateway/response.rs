use bytes::Bytes;
use opentelemetry::trace::Status;
use pingora::http::ResponseHeader;
use pingora::proxy::Session;
use pingora::Result;
use tracing::Span;
use tracing_opentelemetry::OpenTelemetrySpanExt;

use crate::error::ApiError;
use crate::pipeline::Rejection;
use crate::rate_limit::RateLimitInfo;
use crate::route_map::Route;

use super::context::GatewayContext;
use super::cors::CorsPolicy;

/// 5xx → span Error (no description). 4xx stay unset.
pub fn apply_error_span_status(span: &Span, status: u16) {
    span.record("http.response.status_code", status);
    match status {
        503 => {
            span.record("error.type", "network");
            span.set_status(Status::error(""));
        }
        500..=599 => {
            span.record("error.type", "internal");
            span.set_status(Status::error(""));
        }
        _ => {}
    }
}

pub async fn send_json_error(
    cors: &CorsPolicy,
    session: &mut Session,
    ctx: &GatewayContext,
    status: u16,
    error: ApiError,
) -> Result<()> {
    send_json_error_headers(cors, session, ctx, status, error, &[]).await
}

pub async fn send_json_error_headers(
    cors: &CorsPolicy,
    session: &mut Session,
    ctx: &GatewayContext,
    status: u16,
    error: ApiError,
    extra_headers: &[(&'static str, String)],
) -> Result<()> {
    let body = error.to_json_bytes(ctx.request_id.as_deref());
    send_json(cors, session, ctx, status, body, extra_headers).await
}

/// A JSON response the gateway answers itself, with the headers every response
/// gets: security headers, CORS and `X-Request-ID`.
pub async fn send_json(
    cors: &CorsPolicy,
    session: &mut Session,
    ctx: &GatewayContext,
    status: u16,
    body: Vec<u8>,
    extra_headers: &[(&'static str, String)],
) -> Result<()> {
    if let Some(ref span) = ctx.root_span {
        apply_error_span_status(span, status);
    }

    let mut header = ResponseHeader::build(status, None)?;
    header.insert_header("Content-Type", "application/json")?;
    header.insert_header("Content-Length", body.len().to_string())?;
    apply_security_headers(&mut header)?;
    cors.apply(&mut header, ctx.request_origin.as_deref())?;
    if let Some(ref request_id) = ctx.request_id {
        header.insert_header("X-Request-ID", request_id)?;
    }
    for (name, value) in extra_headers {
        header.insert_header(*name, value.clone())?;
    }
    session
        .write_response_header(Box::new(header), false)
        .await?;
    session
        .write_response_body(Some(Bytes::from(body)), true)
        .await?;
    Ok(())
}

pub async fn write_json_error(
    cors: &CorsPolicy,
    session: &mut Session,
    ctx: &GatewayContext,
    status: u16,
    error: ApiError,
) -> Result<bool> {
    send_json_error(cors, session, ctx, status, error).await?;
    Ok(true)
}

pub async fn write_rate_limited(
    cors: &CorsPolicy,
    session: &mut Session,
    ctx: &GatewayContext,
    retry_after_seconds: u64,
    admitted: Option<&RateLimitInfo>,
) -> Result<bool> {
    let mut extras = vec![("Retry-After", retry_after_seconds.to_string())];
    if let Some(info) = admitted {
        extras.push(("X-RateLimit-Limit", info.limit.to_string()));
        extras.push(("X-RateLimit-Remaining", info.remaining.to_string()));
        extras.push(("X-RateLimit-Reset", info.reset_epoch.to_string()));
    }
    send_json_error_headers(
        cors,
        session,
        ctx,
        429,
        ApiError::rate_limited(retry_after_seconds),
        &extras,
    )
    .await?;
    Ok(true)
}

const SECURITY_HEADERS: &[(&str, &str)] = &[
    ("X-Content-Type-Options", "nosniff"),
    ("X-Frame-Options", "DENY"),
    ("Referrer-Policy", "strict-origin-when-cross-origin"),
];

/// Each security header the response hasn't set (docs/reference/gateway.md#security-headers).
pub fn apply_security_headers(header: &mut ResponseHeader) -> Result<()> {
    for (name, value) in SECURITY_HEADERS {
        if !header.headers.contains_key(*name) {
            header.insert_header(*name, *value)?;
        }
    }
    Ok(())
}

pub fn apply_rate_limit_headers(header: &mut ResponseHeader, info: &RateLimitInfo) -> Result<()> {
    header.insert_header("X-RateLimit-Limit", info.limit.to_string())?;
    header.insert_header("X-RateLimit-Remaining", info.remaining.to_string())?;
    header.insert_header("X-RateLimit-Reset", info.reset_epoch.to_string())?;
    Ok(())
}

/// Answer a [`Rejection`] for `route`.
pub async fn write_rejection(
    cors: &CorsPolicy,
    session: &mut Session,
    ctx: &GatewayContext,
    route: &Route,
    rejection: Rejection,
) -> Result<bool> {
    match rejection {
        Rejection::RateLimited { retry_after, info } => {
            write_rate_limited(cors, session, ctx, retry_after, info.as_ref()).await
        }
        other => write_json_error(cors, session, ctx, other.status(), other.body(route)).await,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn security_headers_fill_only_what_the_upstream_left_unset() {
        let mut h = ResponseHeader::build(200, None).unwrap();
        h.insert_header("X-Frame-Options", "SAMEORIGIN").unwrap();
        apply_security_headers(&mut h).unwrap();
        let get = |name: &str| h.headers.get(name).unwrap().to_str().unwrap().to_string();
        assert_eq!(get("X-Frame-Options"), "SAMEORIGIN");
        assert_eq!(get("X-Content-Type-Options"), "nosniff");
        assert_eq!(get("Referrer-Policy"), "strict-origin-when-cross-origin");
    }

    mod root_span {
        use super::super::GatewayContext;
        use super::apply_error_span_status;
        use opentelemetry::trace::{Status, TracerProvider};
        use opentelemetry_sdk::trace::{InMemorySpanExporter, SdkTracerProvider, SpanData};
        use std::collections::HashMap;
        use tracing_subscriber::layer::SubscriberExt;

        /// The gateway's root span for one request that ends with `status`.
        fn root_span(scheme: &str, path: &str, query: Option<&str>, status: u16) -> SpanData {
            let exporter = InMemorySpanExporter::default();
            let provider = SdkTracerProvider::builder()
                .with_simple_exporter(exporter.clone())
                .build();
            let subscriber = tracing_subscriber::registry()
                .with(tracing_opentelemetry::layer().with_tracer(provider.tracer("test")));
            tracing::subscriber::with_default(subscriber, || {
                let mut ctx = GatewayContext::new();
                let span = ctx.ensure_root_span("GET", scheme, path, query);
                apply_error_span_status(&span, status);
                drop(span);
                ctx.finish_root_span();
            });
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

        #[test]
        fn url_query_is_redacted() {
            let span = root_span(
                "https",
                "/orgs/acme/files",
                Some("sig=abc123&page=2&X-Amz-Signature=s3&X-Amz-Credential=c"),
                200,
            );
            let a = attrs(&span);
            assert_eq!(
                a["url.query"],
                "sig=REDACTED&page=2&X-Amz-Signature=REDACTED&X-Amz-Credential=REDACTED"
            );
            assert_eq!(a["url.scheme"], "https");
            assert_eq!(a["url.path"], "/orgs/acme/files");
            assert_eq!(a["http.request.method"], "GET");
        }

        #[test]
        fn no_query_no_url_query() {
            for query in [None, Some("")] {
                let a = attrs(&root_span("http", "/health", query, 200));
                assert!(!a.contains_key("url.query"), "{a:?}");
                assert_eq!(a["url.scheme"], "http");
            }
        }

        #[test]
        fn error_type_on_5xx_only() {
            let span = root_span("http", "/x", None, 500);
            assert_eq!(attrs(&span)["error.type"], "internal");
            assert_eq!(span.status, Status::error(""));
            let span = root_span("http", "/x", None, 503);
            assert_eq!(attrs(&span)["error.type"], "network");

            let span = root_span("http", "/x", None, 404);
            assert!(!attrs(&span).contains_key("error.type"));
            assert_eq!(span.status, Status::Unset);
            assert_eq!(attrs(&span)["http.response.status_code"], "404");
        }
    }
}
