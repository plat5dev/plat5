mod context;
mod cors;
mod response;
mod upstream;

use std::collections::HashMap;
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
use crate::auth::jwt::JwtValidatorState;
use crate::auth::AuthStack;
use crate::config::GatewayConfig;
use crate::error::{ApiError, ErrorKind};
use crate::metrics;
use crate::rate_limit::RateLimiter;
use crate::route_config::{scopes_intersect, RouteRateLimit};
use crate::route_map::{Route, RouteMap, RouteScope};

pub use crate::admission::parse_user_id_claim;
pub use context::GatewayContext;
use cors::CorsPolicy;

const MAX_BODY_SIZE_BYTES: u64 = 10 * 1024 * 1024; // 10 MB

struct RateLimitFallback {
    requests: u64,
    window_seconds: u64,
}

pub struct UserGateway {
    admissor: Admissor,
    route_map: Arc<ArcSwap<RouteMap>>,
    connect_timeout: Duration,
    read_timeout: Duration,
    cors: CorsPolicy,
    route_limiter: RateLimiter,
    auth_failure_limiter: RateLimiter,
    rate_limit_fallback: RateLimitFallback,
    auth_failure_requests: u64,
    auth_failure_window_seconds: u64,
}

impl UserGateway {
    pub fn new(
        cfg: &GatewayConfig,
        jwt_validator: JwtValidatorState,
        route_map: Arc<ArcSwap<RouteMap>>,
    ) -> Self {
        Self {
            admissor: Admissor::new(AuthStack::from_config(cfg, jwt_validator)),
            route_map,
            connect_timeout: cfg.upstream_connect_timeout,
            read_timeout: cfg.upstream_read_timeout,
            cors: CorsPolicy::new(cfg.allowed_origins.clone()),
            route_limiter: RateLimiter::new(),
            auth_failure_limiter: RateLimiter::new(),
            rate_limit_fallback: RateLimitFallback {
                requests: cfg.rate_limit_requests,
                window_seconds: cfg.rate_limit_window_seconds,
            },
            auth_failure_requests: cfg.rate_limit_auth_failure_requests,
            auth_failure_window_seconds: cfg.rate_limit_auth_failure_window_seconds,
        }
    }

    async fn handle_preflight(&self, session: &mut Session, ctx: &GatewayContext) -> Result<bool> {
        let mut header = ResponseHeader::build(200, None)?;
        self.cors
            .apply(&mut header, ctx.request_origin.as_deref())?;
        header.insert_header("Access-Control-Max-Age", "86400")?;
        session
            .write_response_header(Box::new(header), true)
            .await?;
        Ok(true)
    }

    async fn reject_oversized_content_length(
        &self,
        session: &mut Session,
        ctx: &GatewayContext,
    ) -> Result<Option<bool>> {
        let Some(content_length) = session.req_header().headers.get("content-length") else {
            return Ok(None);
        };
        let Ok(len_str) = content_length.to_str() else {
            return Ok(None);
        };
        let Ok(len) = len_str.parse::<u64>() else {
            return Ok(None);
        };
        if len <= MAX_BODY_SIZE_BYTES {
            return Ok(None);
        }
        warn!(
            content_length = len,
            max_body_size = MAX_BODY_SIZE_BYTES,
            "request body too large"
        );
        Ok(Some(
            response::write_json_error(
                &self.cors,
                session,
                ctx,
                413,
                ApiError::payload_too_large(MAX_BODY_SIZE_BYTES),
            )
            .await?,
        ))
    }

    fn resolve_route(&self, path: &str, method: &str) -> Option<(Route, HashMap<String, String>)> {
        let route_map = self.route_map.load();
        route_map
            .find_route(path, method)
            .map(|(route, params)| (route.clone(), params))
    }

    async fn note_unadmitted(
        &self,
        session: &mut Session,
        ctx: &GatewayContext,
    ) -> Result<Option<bool>> {
        match self.auth_failure_limiter.allow(
            &format!("ip:{}", client_ip(session)),
            self.auth_failure_requests,
            self.auth_failure_window_seconds,
        ) {
            Ok(()) => Ok(None),
            Err(retry) => Ok(Some(
                response::write_rate_limited(&self.cors, session, ctx, retry).await?,
            )),
        }
    }

    async fn prepare_upstream(
        &self,
        session: &mut Session,
        ctx: &mut GatewayContext,
        path: &str,
        route: &Route,
        params: &HashMap<String, String>,
        request_id: &str,
    ) -> Result<bool> {
        if let Err(err) = session
            .req_header_mut()
            .insert_header("X-Request-ID", request_id)
        {
            warn!(
                error_kind = ErrorKind::Internal.as_str(),
                error_message = %err,
                "failed to inject X-Request-ID header"
            );
            return response::write_json_error(
                &self.cors,
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
            .admit(session.req_header(), route, params)
            .await
        {
            Ok(a) => a,
            Err(err) => {
                if err.is_unadmitted_401() {
                    if let Some(done) = self.note_unadmitted(session, ctx).await? {
                        return Ok(done);
                    }
                }
                return response::write_admit_error(&self.cors, session, ctx, err).await;
            }
        };

        if let Some(required) = route.required_scopes.as_ref().filter(|s| !s.is_empty()) {
            if let Some(granted) = admission.key_scopes() {
                if !scopes_intersect(required, granted) {
                    return response::write_json_error(
                        &self.cors,
                        session,
                        ctx,
                        403,
                        ApiError::forbidden(Some(serde_json::json!({
                            "permission": "required_scopes",
                            "resource": "route",
                            "resource_id": route.path,
                        }))),
                    )
                    .await;
                }
            }
        }

        if let Some((limit, window)) = effective_rate_limit(route, &self.rate_limit_fallback) {
            let subject = limit_subject(route.scope, &admission, &client_ip(session));
            let key = format!("{} {} {}", ctx.method_label(), route.path, subject);
            if let Err(retry) = self.route_limiter.allow(&key, limit, window) {
                return response::write_rate_limited(&self.cors, session, ctx, retry).await;
            }
        }

        upstream::record_admission_span(ctx, &admission);
        if upstream::apply_admission_headers(session.req_header_mut(), &admission).is_err() {
            return response::write_json_error(
                &self.cors,
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
            path,
            route,
            params,
            self.connect_timeout,
            self.read_timeout,
        )?;
        Ok(false)
    }
}

fn client_ip(session: &Session) -> String {
    let headers = &session.req_header().headers;
    if let Some(v) = headers.get("x-forwarded-for").and_then(|v| v.to_str().ok()) {
        if let Some(first) = v.split(',').next() {
            let t = first.trim();
            if !t.is_empty() {
                return t.to_string();
            }
        }
    }
    if let Some(v) = headers.get("x-real-ip").and_then(|v| v.to_str().ok()) {
        let t = v.trim();
        if !t.is_empty() {
            return t.to_string();
        }
    }
    match session.client_addr() {
        Some(addr) => match addr.as_inet() {
            Some(sock) => sock.ip().to_string(),
            None => addr.to_string(),
        },
        None => "unknown".to_string(),
    }
}

fn effective_rate_limit(route: &Route, fallback: &RateLimitFallback) -> Option<(u64, u64)> {
    match &route.rate_limit {
        Some(RouteRateLimit::Unlimited) => None,
        Some(RouteRateLimit::Limit(cfg)) => Some((cfg.requests, cfg.window_seconds)),
        None => {
            if fallback.requests == 0 {
                None
            } else {
                Some((fallback.requests, fallback.window_seconds))
            }
        }
    }
}

fn limit_subject(scope: RouteScope, admission: &crate::admission::Admission, ip: &str) -> String {
    use crate::admission::Admission;
    match scope {
        RouteScope::Public => format!("ip:{ip}"),
        RouteScope::User => match admission {
            Admission::User { user_id, .. } => format!("user:{user_id}"),
            _ => format!("ip:{ip}"),
        },
        RouteScope::Organization => match admission {
            Admission::Organization {
                organization_id, ..
            } => format!("org:{organization_id}"),
            _ => format!("ip:{ip}"),
        },
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

struct RequestLog<'a> {
    request_id: &'a str,
    route: &'a str,
    method: &'a str,
    status: u16,
    duration_ms: f64,
    trace_id: Option<&'a str>,
    span_id: Option<&'a str>,
}

fn log_request_outcome(log: RequestLog<'_>, error: Option<&pingora::Error>) {
    let RequestLog {
        request_id,
        route,
        method,
        status,
        duration_ms,
        trace_id,
        span_id,
    } = log;

    match (error, trace_id, span_id) {
        (Some(err), Some(trace_id), Some(span_id)) => {
            warn!(
                trace_id,
                span_id,
                request_id,
                route = %route,
                method = %method,
                status,
                duration_ms,
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
                duration_ms,
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
                duration_ms,
                "request completed"
            );
        }
        (None, _, _) => {
            info!(
                request_id,
                route = %route,
                method = %method,
                status,
                duration_ms,
                "request completed"
            );
        }
    }
}

#[async_trait]
impl ProxyHttp for UserGateway {
    type CTX = GatewayContext;

    fn new_ctx(&self) -> Self::CTX {
        GatewayContext::new()
    }

    async fn request_filter(&self, session: &mut Session, ctx: &mut Self::CTX) -> Result<bool> {
        let path = session.req_header().uri.path().to_string();
        let method = session.req_header().method.as_str().to_string();
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

        if session.req_header().method == http::Method::OPTIONS {
            return self.handle_preflight(session, ctx).await;
        }

        if let Some(done) = self.reject_oversized_content_length(session, ctx).await? {
            return Ok(done);
        }

        let Some((route, params)) = self.resolve_route(&path, &method) else {
            warn!("no matching route");
            if let Some(done) = self.note_unadmitted(session, ctx).await? {
                return Ok(done);
            }
            return response::write_json_error(
                &self.cors,
                session,
                ctx,
                404,
                ApiError::not_found(),
            )
            .await;
        };

        ctx.route = Some(route.path.clone());
        self.prepare_upstream(session, ctx, &path, &route, &params, &request_id)
            .await
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
            if let Err(err) = response::send_json_error(&self.cors, session, ctx, code, error).await
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
        self.cors
            .apply(upstream_response, ctx.request_origin.as_deref())?;

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
            } else if matches!(status, 400 | 401 | 403 | 404 | 413 | 429) {
                span.set_status(Status::Ok);
            }
        }

        metrics::record_request(&route, &method, status, duration);

        let request_id = ctx.request_id.as_deref().unwrap_or("");
        let (trace_id, span_id) = otel_trace_ids(root_span.as_ref());
        log_request_outcome(
            RequestLog {
                request_id,
                route: &route,
                method: &method,
                status,
                duration_ms: duration * 1000.0,
                trace_id: trace_id.as_deref(),
                span_id: span_id.as_deref(),
            },
            e,
        );

        ctx.finish_root_span();
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::admission::{Admission, OrgVia};
    use crate::auth::AuthType;

    fn org_admission(org: &str, member: &str, member_key: bool) -> Admission {
        Admission::Organization {
            organization_id: org.into(),
            member_id: member.into(),
            via: if member_key {
                OrgVia::MemberKey
            } else {
                OrgVia::User {
                    user_id: "user-1".into(),
                    auth_type: AuthType::Jwt,
                    kid: None,
                }
            },
            key_scopes: None,
        }
    }

    #[test]
    fn public_scope_limits_by_ip() {
        assert_eq!(
            limit_subject(RouteScope::Public, &Admission::Public, "1.2.3.4"),
            "ip:1.2.3.4"
        );
    }

    #[test]
    fn user_scope_limits_by_user_id() {
        let admission = Admission::User {
            user_id: "user-9".into(),
            auth_type: AuthType::Jwt,
            kid: None,
            key_scopes: None,
        };
        assert_eq!(
            limit_subject(RouteScope::User, &admission, "1.2.3.4"),
            "user:user-9"
        );
    }

    #[test]
    fn organization_scope_limits_by_org_id() {
        let jwt = org_admission("org-1", "member-9", false);
        assert_eq!(
            limit_subject(RouteScope::Organization, &jwt, "1.2.3.4"),
            "org:org-1"
        );
        let sa = org_admission("org-1", "sa-member", true);
        assert_eq!(
            limit_subject(RouteScope::Organization, &sa, "9.9.9.9"),
            "org:org-1"
        );
    }
}
