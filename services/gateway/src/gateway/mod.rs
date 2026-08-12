mod context;
mod cors;
mod response;
mod upstream;

use std::sync::Arc;
use std::time::Duration;

use arc_swap::ArcSwap;
use async_trait::async_trait;
use bytes::Bytes;
use opentelemetry::trace::{Status, TraceContextExt};
use pingora::http::ResponseHeader;
use pingora::proxy::{FailToProxy, ProxyHttp, Session};
use pingora::upstreams::peer::HttpPeer;
use pingora::{Error, ErrorSource, ErrorType, Result};
use tracing::{debug, info, warn};
use tracing_opentelemetry::OpenTelemetrySpanExt;
use uuid::Uuid;

use crate::admission::Admissor;
use crate::auth::jwt::{JwtCache, JwtValidatorState};
use crate::auth::member_apikey::{MemberApiKeyCache, MemberApiKeyValidator};
use crate::auth::membership::{MembershipCache, MembershipResolver};
use crate::auth::user_apikey::{UserApiKeyCache, UserApiKeyValidator};
use crate::config::GatewayConfig;
use crate::error::{ApiError, ErrorKind};
use crate::internal_http::InternalHttpClient;
use crate::metrics;
use crate::route_map::RouteMap;

pub use crate::admission::parse_user_id_claim;
pub use context::GatewayContext;

const JWT_CACHE_CAPACITY: u64 = 10_000;
const JWT_CACHE_TTL_BUFFER_SECS: u64 = 60;
const USER_APIKEY_CACHE_CAPACITY: u64 = 10_000;
const MEMBER_APIKEY_CACHE_CAPACITY: u64 = 10_000;
const MEMBERSHIP_CACHE_CAPACITY: u64 = 10_000;
const MAX_BODY_SIZE_BYTES: u64 = 10 * 1024 * 1024; // 10 MB

pub struct UserGateway {
    admissor: Admissor,
    route_map: Arc<ArcSwap<RouteMap>>,
    connect_timeout: Duration,
    read_timeout: Duration,
    /// Empty = reflect `*`. Non-empty = allowlist; matched Origin is reflected with `Vary: Origin`.
    allowed_origins: Vec<String>,
}

impl UserGateway {
    pub fn new(
        cfg: &GatewayConfig,
        jwt_validator: JwtValidatorState,
        route_map: Arc<ArcSwap<RouteMap>>,
    ) -> Self {
        let http = InternalHttpClient::new(cfg.internal_auth_token.clone());

        let user_apikey_validator =
            UserApiKeyValidator::new(cfg.user_apikey_validate_url.clone(), http.clone());
        let member_apikey_validator = cfg
            .member_apikey_validate_url
            .clone()
            .map(|url| MemberApiKeyValidator::new(url, http.clone()));
        let membership_resolver = cfg
            .membership_resolve_url
            .clone()
            .map(|url| MembershipResolver::new(url, http));

        let admissor = Admissor::new(
            jwt_validator,
            JwtCache::new(JWT_CACHE_CAPACITY, JWT_CACHE_TTL_BUFFER_SECS),
            cfg.auth_user_id_claim.clone(),
            user_apikey_validator,
            UserApiKeyCache::new(USER_APIKEY_CACHE_CAPACITY, cfg.apikey_cache_ttl_secs),
            member_apikey_validator,
            MemberApiKeyCache::new(MEMBER_APIKEY_CACHE_CAPACITY, cfg.apikey_cache_ttl_secs),
            membership_resolver,
            MembershipCache::new(MEMBERSHIP_CACHE_CAPACITY, cfg.membership_cache_ttl_secs),
        );

        Self {
            admissor,
            route_map,
            connect_timeout: cfg.upstream_connect_timeout,
            read_timeout: cfg.upstream_read_timeout,
            allowed_origins: cfg.allowed_origins.clone(),
        }
    }
}

fn otel_trace_ids(span: Option<&tracing::Span>) -> (Option<String>, Option<String>) {
    let Some(span) = span else {
        return (None, None);
    };
    let cx = span.context();
    let sc = cx.span().span_context().clone();
    if !sc.is_valid() {
        return (None, None);
    }
    (
        Some(sc.trace_id().to_string()),
        Some(sc.span_id().to_string()),
    )
}

#[async_trait]
impl ProxyHttp for UserGateway {
    type CTX = GatewayContext;

    fn new_ctx(&self) -> Self::CTX {
        GatewayContext::new()
    }

    async fn request_filter(&self, session: &mut Session, ctx: &mut Self::CTX) -> Result<bool> {
        let path = session.req_header().uri.path().to_string();
        let method = session.req_header().method.to_string();
        ctx.method = Some(method.clone());
        ctx.request_origin = session
            .req_header()
            .headers
            .get("origin")
            .and_then(|v| v.to_str().ok())
            .map(|s| s.to_string());
        let root_span = ctx.ensure_root_span(&path, &method);
        let _root_guard = root_span.enter();

        let span = tracing::info_span!("gateway.request_filter");
        let _entered = span.enter();

        let request_id = Uuid::new_v4().to_string();
        ctx.request_id = Some(request_id.clone());
        if let Some(ref span) = ctx.root_span {
            span.record("request_id", request_id.as_str());
        }

        if method == "OPTIONS" {
            let mut header = ResponseHeader::build(200, None)?;
            cors::apply_cors(
                &self.allowed_origins,
                &mut header,
                ctx.request_origin.as_deref(),
            )?;
            header.insert_header("Access-Control-Max-Age", "86400")?;
            session
                .write_response_header(Box::new(header), true)
                .await?;
            return Ok(true);
        }

        // Content-Length early reject (chunked bodies enforced in request_body_filter)
        if let Some(content_length) = session.req_header().headers.get("content-length") {
            if let Ok(len_str) = content_length.to_str() {
                if let Ok(len) = len_str.parse::<u64>() {
                    if len > MAX_BODY_SIZE_BYTES {
                        warn!(
                            content_length = len,
                            max_body_size = MAX_BODY_SIZE_BYTES,
                            "request body too large"
                        );
                        return response::write_json_error(
                            &self.allowed_origins,
                            session,
                            ctx,
                            413,
                            ApiError::payload_too_large(MAX_BODY_SIZE_BYTES),
                        )
                        .await;
                    }
                }
            }
        }

        let (route, params) = {
            let route_map = self.route_map.load();
            match route_map.find_route(&path, &method) {
                Some((route, params)) => (route.clone(), params),
                None => {
                    warn!("no matching route");
                    return response::write_json_error(
                        &self.allowed_origins,
                        session,
                        ctx,
                        404,
                        ApiError::not_found(),
                    )
                    .await;
                }
            }
        };

        ctx.route = Some(route.path.clone());

        if let Err(err) = session
            .req_header_mut()
            .insert_header("X-Request-ID", &request_id)
        {
            warn!(
                error_kind = ErrorKind::Internal.as_str(),
                error_message = %err,
                "failed to inject X-Request-ID header"
            );
            return response::write_json_error(
                &self.allowed_origins,
                session,
                ctx,
                500,
                ApiError::internal_error(),
            )
            .await;
        }

        upstream::strip_identity_headers(session.req_header_mut());

        let admission = match self
            .admissor
            .admit(session.req_header(), &route, &params)
            .await
        {
            Ok(a) => a,
            Err(err) => {
                return response::write_admit_error(&self.allowed_origins, session, ctx, err)
                    .await;
            }
        };

        upstream::record_admission_span(ctx, &admission);
        if let Err(_err) = upstream::apply_admission_headers(session.req_header_mut(), &admission) {
            return response::write_json_error(
                &self.allowed_origins,
                session,
                ctx,
                500,
                ApiError::internal_error(),
            )
            .await;
        }

        upstream::build_and_store_upstream_peer(
            session,
            ctx,
            &path,
            &route,
            &params,
            self.connect_timeout,
            self.read_timeout,
        )?;
        Ok(false)
    }

    async fn request_body_filter(
        &self,
        _session: &mut Session,
        body: &mut Option<Bytes>,
        _end_of_stream: bool,
        ctx: &mut Self::CTX,
    ) -> Result<()> {
        if let Some(chunk) = body {
            ctx.body_bytes = ctx.body_bytes.saturating_add(chunk.len() as u64);
            if ctx.body_bytes > MAX_BODY_SIZE_BYTES {
                warn!(
                    body_bytes = ctx.body_bytes,
                    max_body_size = MAX_BODY_SIZE_BYTES,
                    "request body exceeded maximum during streaming"
                );
                return Err(Error::new(ErrorType::HTTPStatus(413)));
            }
        }
        Ok(())
    }

    async fn fail_to_proxy(
        &self,
        session: &mut Session,
        e: &Error,
        ctx: &mut Self::CTX,
    ) -> FailToProxy {
        let code = match e.etype() {
            ErrorType::HTTPStatus(code) => *code,
            _ => match e.esource() {
                ErrorSource::Upstream => 502,
                ErrorSource::Downstream => match e.etype() {
                    ErrorType::WriteError | ErrorType::ReadError | ErrorType::ConnectionClosed => 0,
                    _ => 400,
                },
                ErrorSource::Internal | ErrorSource::Unset => 500,
            },
        };

        if code > 0 && session.response_written().is_none() {
            let error = match code {
                413 => ApiError::payload_too_large(MAX_BODY_SIZE_BYTES),
                502 => ApiError::service_unavailable(),
                _ => ApiError::internal_error(),
            };
            if let Err(err) =
                response::send_json_error(&self.allowed_origins, session, ctx, code, error).await
            {
                warn!(
                    status = code,
                    error = %err,
                    "failed to send error response to downstream"
                );
            }
        }

        FailToProxy {
            error_code: code,
            can_reuse_downstream: false,
        }
    }

    async fn upstream_peer(
        &self,
        _session: &mut Session,
        ctx: &mut Self::CTX,
    ) -> Result<Box<HttpPeer>> {
        let root_span = ctx.root_span();
        let _root_guard = root_span.as_ref().map(|span| span.enter());

        if let Some(peer) = ctx.upstream_peer.take() {
            return Ok(peer);
        }

        warn!("upstream_peer called without pre-resolved route");
        Err(Error::new(ErrorType::HTTPStatus(500)))
    }

    async fn response_filter(
        &self,
        _session: &mut Session,
        upstream_response: &mut ResponseHeader,
        ctx: &mut Self::CTX,
    ) -> Result<()> {
        let root_span = ctx.root_span();
        let _root_guard = root_span.as_ref().map(|span| span.enter());

        if let Some(ref request_id) = ctx.request_id {
            upstream_response.insert_header("X-Request-ID", request_id)?;
        }

        upstream_response.remove_header("alt-svc");
        cors::apply_cors(
            &self.allowed_origins,
            upstream_response,
            ctx.request_origin.as_deref(),
        )?;

        debug!("rewrote upstream response headers");
        Ok(())
    }

    async fn logging(
        &self,
        session: &mut Session,
        e: Option<&pingora::Error>,
        ctx: &mut Self::CTX,
    ) {
        let root_span = ctx.root_span();
        let _root_guard = root_span.as_ref().map(|span| span.enter());

        let status = session
            .response_written()
            .map_or(0, |resp| resp.status.as_u16());
        let duration = ctx.start.elapsed().as_secs_f64();
        let route = ctx.route_label().to_string();
        let method = ctx.method_label().to_string();

        if let Some(ref span) = ctx.root_span {
            span.record("http.status_code", status);
            if let Some(err) = e {
                span.record("error.kind", "network");
                span.set_status(Status::error(err.to_string()));
            } else if matches!(status, 500..=599) {
                span.record("error.kind", "internal");
                span.set_status(Status::error(format!("http {status}")));
            } else if matches!(status, 400 | 401 | 404 | 413) {
                span.set_status(Status::Ok);
            }
        }

        metrics::record_request(&route, &method, status, duration);

        let request_id = ctx.request_id.as_deref().unwrap_or("");
        let (trace_id, span_id) = otel_trace_ids(root_span.as_ref());

        match (e, trace_id.as_deref(), span_id.as_deref()) {
            (Some(err), Some(trace_id), Some(span_id)) => {
                warn!(
                    trace_id,
                    span_id,
                    request_id,
                    route = %route,
                    method = %method,
                    status,
                    duration_ms = duration * 1000.0,
                    error = %err,
                    "request failed"
                );
            }
            (Some(err), _, _) => {
                warn!(
                    request_id,
                    route = %route,
                    method = %method,
                    status,
                    duration_ms = duration * 1000.0,
                    error = %err,
                    "request failed"
                );
            }
            (None, Some(trace_id), Some(span_id)) => {
                info!(
                    trace_id,
                    span_id,
                    request_id,
                    route = %route,
                    method = %method,
                    status,
                    duration_ms = duration * 1000.0,
                    "request completed"
                );
            }
            (None, _, _) => {
                info!(
                    request_id,
                    route = %route,
                    method = %method,
                    status,
                    duration_ms = duration * 1000.0,
                    "request completed"
                );
            }
        }

        ctx.finish_root_span();
    }
}
