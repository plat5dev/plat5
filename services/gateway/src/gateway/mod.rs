mod context;
mod cors;
mod labels;
mod response;
mod rewrite;
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

use crate::admission::{Admissor, RequestCredential};
use crate::audit::{self, Audit, AuditState};
use crate::auth::jwt::JwtValidatorState;
use crate::auth::AuthStack;
use crate::config::GatewayConfig;
use crate::error::{ApiError, ErrorKind};
use crate::metrics;
use crate::rate_limit::{RateLimitError, RateLimiter};
use crate::route_map::{LimitBucket, Route, RouteLimiter, RouteMap, RouteScope};
use labels::satisfies_required_labels;

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
    limiter: RateLimiter,
    rate_limit_fallback: RateLimitFallback,
    auth_failure_requests: u64,
    auth_failure_window_seconds: u64,
    audit: Audit,
}

impl UserGateway {
    pub fn new(
        cfg: &GatewayConfig,
        jwt_validator: JwtValidatorState,
        route_map: Arc<ArcSwap<RouteMap>>,
        limiter: RateLimiter,
        audit: Audit,
    ) -> Self {
        Self {
            admissor: Admissor::new(AuthStack::from_config(cfg, jwt_validator)),
            route_map,
            connect_timeout: cfg.upstream_connect_timeout,
            read_timeout: cfg.upstream_read_timeout,
            cors: CorsPolicy::new(cfg.allowed_origins.clone()),
            limiter,
            rate_limit_fallback: RateLimitFallback {
                requests: cfg.rate_limit_requests,
                window_seconds: cfg.rate_limit_window_seconds,
            },
            auth_failure_requests: cfg.rate_limit_auth_failure_requests,
            auth_failure_window_seconds: cfg.rate_limit_auth_failure_window_seconds,
            audit,
        }
    }

    /// CORS preflight: 204 with no body. A 204 has no body by definition, so the
    /// response is complete after the headers and the connection can be reused.
    async fn handle_preflight(&self, session: &mut Session, ctx: &GatewayContext) -> Result<bool> {
        let mut header = ResponseHeader::build(204, None)?;
        response::apply_security_headers(&mut header)?;
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
        match self
            .limiter
            .allow(
                &format!("ip:{}", client_ip(session)),
                self.auth_failure_requests,
                self.auth_failure_window_seconds,
            )
            .await
        {
            Ok(_) => Ok(None),
            Err(RateLimitError::Exceeded { retry_after, .. }) => Ok(Some(
                response::write_rate_limited(&self.cors, session, ctx, retry_after, None).await?,
            )),
            Err(RateLimitError::Unavailable) => Ok(Some(
                response::write_json_error(
                    &self.cors,
                    session,
                    ctx,
                    503,
                    ApiError::service_unavailable(),
                )
                .await?,
            )),
        }
    }

    async fn prepare_upstream(
        &self,
        session: &mut Session,
        ctx: &mut GatewayContext,
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

        let root_span = ctx.root_span();
        let admission = match self
            .admissor
            .admit(
                session.req_header(),
                route,
                root_span.as_ref(),
                &mut ctx.credential,
            )
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

        if let Some((limit, window, bucket)) = limit_plan(
            route,
            &self.rate_limit_fallback,
            ctx.method_label(),
            &admission,
            &client_ip(session),
        ) {
            match self.limiter.allow(&bucket, limit, window).await {
                Ok(info) => {
                    ctx.rate_limit = Some(info);
                }
                Err(RateLimitError::Exceeded { retry_after, info }) => {
                    return response::write_rate_limited(
                        &self.cors,
                        session,
                        ctx,
                        retry_after,
                        Some(&info),
                    )
                    .await;
                }
                Err(RateLimitError::Unavailable) => {
                    return response::write_json_error(
                        &self.cors,
                        session,
                        ctx,
                        503,
                        ApiError::service_unavailable(),
                    )
                    .await;
                }
            }
        }

        // Audited: the intent is written before anything past the limiter can answer,
        // and nothing reaches the service unless it is (docs/audit.md#delivery).
        if self.audit.enabled() && route.audited(ctx.method_label()) {
            ctx.audit = Some(AuditState::default());
            let written = match audit_intent(session, ctx, route, params, request_id) {
                Some(intent) => self.audit.write_intent(&intent).await.is_ok(),
                None => false,
            };
            if !written {
                return response::write_json_error(
                    &self.cors,
                    session,
                    ctx,
                    503,
                    ApiError::service_unavailable(),
                )
                .await;
            }
        }

        // After the limiter, so a label-denied request still spends budget.
        if !satisfies_required_labels(route.required_labels.as_deref(), admission.labels()) {
            return response::write_json_error(
                &self.cors,
                session,
                ctx,
                403,
                ApiError::forbidden(Some(serde_json::json!({
                    "permission": "required_labels",
                    "resource": "route",
                    "resource_id": route.path,
                    // Any one of these would have admitted the request.
                    "required_labels": route.required_labels,
                }))),
            )
            .await;
        }

        upstream::record_admission_span(ctx, &admission);

        if let Err(err) = upstream::build_and_store_upstream_peer(
            session,
            ctx,
            route,
            params,
            &admission,
            self.connect_timeout,
            self.read_timeout,
        )
        .await
        {
            let (status, body) = match err {
                rewrite::RewriteError::PathParam => (400, ApiError::invalid_request()),
                rewrite::RewriteError::Subject | rewrite::RewriteError::Internal => {
                    (500, ApiError::internal_error())
                }
                rewrite::RewriteError::BadUpstream => (502, ApiError::internal_error()),
            };
            return response::write_json_error(&self.cors, session, ctx, status, body).await;
        }
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

/// The intent for an audited request. The route is `organization` or `member`, so
/// the credential is an admitted member's. None if it is missing a field.
fn audit_intent(
    session: &Session,
    ctx: &GatewayContext,
    route: &Route,
    params: &HashMap<String, String>,
    request_id: &str,
) -> Option<audit::Intent> {
    let cred = &ctx.credential;
    let (Some(organization_id), Some(member_id), Some(auth_type), Some(key_prefix)) = (
        cred.organization_id.clone(),
        cred.member_id.clone(),
        cred.auth_type,
        cred.key_prefix.clone(),
    ) else {
        warn!(
            error_kind = ErrorKind::Internal.as_str(),
            "admitted credential is missing a field the audit intent needs"
        );
        return None;
    };
    Some(audit::Intent {
        request_id: request_id.to_string(),
        occurred_at: audit::rfc3339_millis(ctx.received_at),
        organization_id,
        actor: audit::Actor {
            member_id,
            auth_type: auth_type.as_str(),
            key_prefix,
        },
        service: route.service.clone(),
        method: ctx.method_label().to_string(),
        route: route.path.clone(),
        params: params.clone(),
        ip: client_ip(session),
        user_agent: session
            .req_header()
            .headers
            .get("user-agent")
            .map(|v| audit::user_agent(v.as_bytes())),
    })
}

fn limit_plan(
    route: &Route,
    fallback: &RateLimitFallback,
    method: &str,
    admission: &crate::admission::Admission,
    ip: &str,
) -> Option<(u64, u64, String)> {
    let subject = limit_subject(route.scope, admission, ip);
    match &route.limiter {
        RouteLimiter::Unlimited => None,
        RouteLimiter::Limited(bound) => {
            let bucket = match &bound.bucket {
                LimitBucket::MethodPath => {
                    format!("{} {} {}", method, route.path, subject)
                }
                LimitBucket::Named(prefix) => format!("{prefix}:{subject}"),
            };
            Some((bound.requests, bound.window_seconds, bucket))
        }
        RouteLimiter::Inherit => {
            if fallback.requests == 0 {
                None
            } else {
                Some((
                    fallback.requests,
                    fallback.window_seconds,
                    format!("{} {} {}", method, route.path, subject),
                ))
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
        RouteScope::Member => match admission {
            Admission::Member { member_id, .. } => format!("member:{member_id}"),
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
    credential: &'a RequestCredential,
}

/// One line per request. Credential fields are present only when known: `key_prefix`
/// as soon as an `X-API-Key` with a known wire prefix is presented (so 401 and 403
/// carry it), the principal ids once admitted. Never the key or token itself.
fn log_request_outcome(log: RequestLog<'_>, error: Option<&pingora::Error>) {
    let RequestLog {
        request_id,
        route,
        method,
        status,
        duration_ms,
        trace_id,
        span_id,
        credential,
    } = log;
    let auth_type = credential.auth_type.map(|t| t.as_str());
    let key_prefix = credential.key_prefix.as_deref();
    let user_id = credential.user_id.as_deref();
    let organization_id = credential.organization_id.as_deref();
    let member_id = credential.member_id.as_deref();

    match error {
        Some(err) => {
            warn!(
                trace_id,
                span_id,
                request_id,
                route = %route,
                method = %method,
                status,
                duration_ms,
                auth_type,
                key_prefix,
                user_id,
                organization_id,
                member_id,
                error = %err,
                "request failed"
            );
        }
        None => {
            info!(
                trace_id,
                span_id,
                request_id,
                route = %route,
                method = %method,
                status,
                duration_ms,
                auth_type,
                key_prefix,
                user_id,
                organization_id,
                member_id,
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
        if let Some(ref span) = ctx.root_span {
            span.record("http.route", route.path.as_str());
            span.context()
                .span()
                .update_name(format!("{} {}", method, route.path));
        }
        self.prepare_upstream(session, ctx, &route, &params, &request_id)
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
            ctx.forwarded = true;
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

        response::apply_security_headers(upstream_response)?;
        if let Some(ref info) = ctx.rate_limit {
            response::apply_rate_limit_headers(upstream_response, info)?;
        }

        upstream_response.remove_header("alt-svc");

        // The service's audit details never reach the client, audited route or not.
        let details = upstream_response
            .headers
            .get(audit::DETAILS_HEADER)
            .map(|v| v.as_bytes().to_vec());
        upstream_response.remove_header(audit::DETAILS_HEADER);
        if let Some(state) = ctx.audit.as_mut() {
            state.upstream_status = Some(upstream_response.status.as_u16());
            if let Some(raw) = details {
                state.details = audit::parse_details(&raw);
                if state.details.is_none() {
                    warn!(
                        route = %ctx.route_label(),
                        "ignoring audit details that are not one ASCII JSON object within 4096 bytes"
                    );
                }
            }
        }
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
            span.record("http.response.status_code", status);
            if e.is_some() {
                span.record("error.kind", "network");
                span.set_status(Status::error(""));
            } else if matches!(status, 500..=599) {
                let kind = if status == 503 { "network" } else { "internal" };
                span.record("error.kind", kind);
                span.set_status(Status::error(""));
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
                credential: &ctx.credential,
            },
            e,
        );

        if let Some(state) = ctx.audit.take() {
            let written = session.response_written().map(|r| r.status.as_u16());
            let outcome = audit::outcome_for(state, ctx.forwarded, written);
            self.audit.send_outcome(request_id.to_string(), outcome);
        }

        ctx.finish_root_span();
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::admission::Admission;
    use crate::auth::AuthType;

    fn org_admission(org: &str) -> Admission {
        Admission::Organization {
            organization_id: org.into(),
            labels: Vec::new(),
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
        };
        assert_eq!(
            limit_subject(RouteScope::User, &admission, "1.2.3.4"),
            "user:user-9"
        );
    }

    #[test]
    fn organization_scope_limits_by_org_id() {
        let admission = org_admission("org-1");
        assert_eq!(
            limit_subject(RouteScope::Organization, &admission, "1.2.3.4"),
            "org:org-1"
        );
    }

    #[test]
    fn member_scope_limits_by_member_id() {
        let admission = Admission::Member {
            organization_id: "org-1".into(),
            member_id: "mem-9".into(),
            labels: Vec::new(),
        };
        assert_eq!(
            limit_subject(RouteScope::Member, &admission, "1.2.3.4"),
            "member:mem-9"
        );
    }
}
