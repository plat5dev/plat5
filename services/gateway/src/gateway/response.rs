use bytes::Bytes;
use opentelemetry::trace::Status;
use pingora::http::ResponseHeader;
use pingora::proxy::Session;
use pingora::Result;
use tracing::{info, warn, Span};
use tracing_opentelemetry::OpenTelemetrySpanExt;

use crate::admission::{AdmitError, AuthError, AuthType};
use crate::error::{ApiError, ErrorKind};
use crate::metrics;
use crate::rate_limit::RateLimitInfo;

use super::context::GatewayContext;
use super::cors::CorsPolicy;

/// 5xx → span Error (no description). 4xx stay unset.
pub fn apply_error_span_status(span: &Span, status: u16) {
    span.record("http.response.status_code", status);
    match status {
        503 => {
            span.record("error.kind", "network");
            span.set_status(Status::error(""));
        }
        500..=599 => {
            span.record("error.kind", "internal");
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

pub fn apply_security_headers(header: &mut ResponseHeader) -> Result<()> {
    header.insert_header("X-Content-Type-Options", "nosniff")?;
    header.insert_header("X-Frame-Options", "DENY")?;
    header.insert_header("Referrer-Policy", "strict-origin-when-cross-origin")?;
    Ok(())
}

pub fn apply_rate_limit_headers(header: &mut ResponseHeader, info: &RateLimitInfo) -> Result<()> {
    header.insert_header("X-RateLimit-Limit", info.limit.to_string())?;
    header.insert_header("X-RateLimit-Remaining", info.remaining.to_string())?;
    header.insert_header("X-RateLimit-Reset", info.reset_epoch.to_string())?;
    Ok(())
}

pub async fn write_admit_error(
    cors: &CorsPolicy,
    session: &mut Session,
    ctx: &GatewayContext,
    err: AdmitError,
) -> Result<bool> {
    match err {
        AdmitError::Auth(auth_err) => write_auth_error(cors, session, ctx, auth_err).await,
        AdmitError::MemberApiKeyInvalid => {
            let auth_type = AuthType::MemberApiKey.as_str();
            metrics::record_auth_failure(auth_type, "invalid_member_apikey");
            info!(
                error_kind = ErrorKind::Auth.as_str(),
                auth_type,
                error_message = "invalid_member_apikey",
                "authentication failed"
            );
            write_json_error(cors, session, ctx, 401, ApiError::unauthorized(None)).await
        }
        AdmitError::NotFound => {
            write_json_error(cors, session, ctx, 404, ApiError::not_found()).await
        }
        AdmitError::Unavailable => {
            write_json_error(cors, session, ctx, 503, ApiError::service_unavailable()).await
        }
        AdmitError::Internal(msg) => {
            warn!(reason = msg, "admission internal error (config bug)");
            write_json_error(cors, session, ctx, 500, ApiError::internal_error()).await
        }
    }
}

async fn write_auth_error(
    cors: &CorsPolicy,
    session: &mut Session,
    ctx: &GatewayContext,
    err: AuthError,
) -> Result<bool> {
    let auth_type = err.auth_type().as_str();
    if err.is_client_error() {
        metrics::record_auth_failure(auth_type, err.as_str());
        info!(
            error_kind = ErrorKind::Auth.as_str(),
            auth_type,
            error_message = err.as_str(),
            "authentication failed"
        );
        write_json_error(
            cors,
            session,
            ctx,
            401,
            ApiError::unauthorized(err.details()),
        )
        .await
    } else {
        warn!(
            error_kind = ErrorKind::Network.as_str(),
            auth_type,
            error_message = err.as_str(),
            "authentication infrastructure unavailable"
        );
        write_json_error(cors, session, ctx, 503, ApiError::service_unavailable()).await
    }
}
