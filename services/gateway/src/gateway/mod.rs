mod client_ip;
mod context;
mod cors;
mod response;
mod upstream;

use std::net::IpAddr;
use std::sync::Arc;

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

use crate::audit::{self, Audit, AuditState};
use crate::auth::admit::{AdmitCache, HttpAdmit};
use crate::auth::jwt::JwtValidatorState;
use crate::auth::{AuthError, Authenticator, NameClaims};
use crate::config::GatewayConfig;
use crate::control_plane::Snapshot;
use crate::error::{ApiError, ErrorKind};
use crate::internal_http::InternalHttpClient;
use crate::metrics;
use crate::pipeline::{
    authorize, forward, parse_checks, Admitted, Caller, GatewayPipeline, Limit, Pipeline,
    Rejection, RequestCredential, MAX_CHECKS_BODY,
};
use crate::rate_limit::RateLimiter;
use crate::route_map::{Matched, Required, Route, Scope};

pub use client_ip::TrustedProxies;
pub use context::GatewayContext;
pub use cors::CorsPolicy;

pub struct Gateway {
    authenticator: Authenticator,
    pipeline: GatewayPipeline,
    /// Routes, rate limits and CORS from the control plane, swapped whole on each change.
    snapshot: Arc<ArcSwap<Snapshot>>,
    audit: Audit,
    trusted_proxies: TrustedProxies,
}

impl Gateway {
    pub fn new(
        cfg: &GatewayConfig,
        jwt_validator: JwtValidatorState,
        snapshot: Arc<ArcSwap<Snapshot>>,
        limiter: RateLimiter,
        audit: Audit,
    ) -> Self {
        let admit = HttpAdmit::new(&cfg.identity_internal_url, InternalHttpClient::new());
        Self {
            authenticator: Authenticator::new(jwt_validator, cfg.auth_subject_claim.clone()),
            pipeline: Pipeline::new(
                AdmitCache::new(admit, cfg.admit_cache_ttl, cfg.admit_negative_cache_ttl),
                limiter,
                Limit {
                    requests: cfg.rate_limit_auth_failure_requests,
                    window_seconds: cfg.rate_limit_auth_failure_window_seconds,
                },
            ),
            snapshot,
            audit,
            trusted_proxies: cfg.trusted_proxies.clone(),
        }
    }

    /// The current CORS policy. Each response reads it once.
    fn cors(&self) -> Arc<CorsPolicy> {
        self.snapshot.load().cors.clone()
    }

    /// CORS preflight: 204 with no body. A 204 has no body by definition, so the
    /// response is complete after the headers and the connection can be reused.
    async fn handle_preflight(&self, session: &mut Session, ctx: &GatewayContext) -> Result<bool> {
        let mut header = ResponseHeader::build(204, None)?;
        response::apply_security_headers(&mut header)?;
        self.cors()
            .apply_preflight(&mut header, ctx.request_origin.as_deref())?;
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
        if len <= ctx.max_body_bytes {
            return Ok(None);
        }
        warn!(
            content_length = len,
            max_body_size = ctx.max_body_bytes,
            "request body too large"
        );
        Ok(Some(
            response::write_json_error(
                &self.cors(),
                session,
                ctx,
                413,
                ApiError::payload_too_large(ctx.max_body_bytes),
            )
            .await?,
        ))
    }

    /// The matched route and the name claims, from one snapshot.
    fn resolve_route(&self, path: &str, method: &str) -> Option<(Matched, Arc<NameClaims>)> {
        let snapshot = self.snapshot.load();
        snapshot
            .routes
            .find_route(path, method)
            .map(|matched| (matched, snapshot.name_claims.clone()))
    }

    async fn prepare_upstream(
        &self,
        session: &mut Session,
        ctx: &mut GatewayContext,
        matched: &Matched,
        name_claims: &NameClaims,
        request_id: &str,
    ) -> Result<bool> {
        let route = &matched.route;
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
                &self.cors(),
                session,
                ctx,
                500,
                ApiError::internal_error(),
            )
            .await;
        }

        let ip = client_ip(session, &self.trusted_proxies);
        let presented = match route.scope {
            // No credential is read on a public route.
            Scope::Public => Err(AuthError::MissingAuthorization),
            _ => {
                self.authenticator
                    .present(session.req_header(), name_claims)
                    .await
            }
        };
        // request_filter has entered the root span: a JWT's kid is recorded on it.
        let decided = self
            .pipeline
            .admit_and_limit(matched, &ip, presented, &mut ctx.credential)
            .await;
        let passed = match decided {
            Ok(passed) => passed,
            Err(rejection) => {
                return response::write_rejection(&self.cors(), session, ctx, route, rejection)
                    .await
            }
        };
        ctx.rate_limit = passed.rate_limit;
        let admitted = passed.admitted;
        upstream::record_admission_span(ctx, &admitted);
        let caller = match &admitted {
            Admitted::Caller(c) => Some(c),
            Admitted::Public => None,
        };
        let audited = self.audit.enabled() && route.audit;

        // After the limiter, so a denied request still spends budget.
        let forwarding = authorize(route, &admitted).and_then(|()| forward(matched, &admitted));
        let forwarding = match forwarding {
            Ok(f) => f,
            Err(rejection) => {
                if let Some(caller) = caller {
                    if rejection == Rejection::Denied {
                        log_permission_denied(ctx, route, caller);
                    }
                    // A refusal after admit is one background event.
                    if audited {
                        let intent = audit_intent(session, ctx, matched, caller, request_id, &ip);
                        self.audit
                            .send_rejected(intent, rejection.status(), request_context(ctx));
                    }
                }
                return response::write_rejection(&self.cors(), session, ctx, route, rejection)
                    .await;
            }
        };

        // Audited: nothing reaches the service unless the intent is written
        // (docs/concepts/audit.md#delivery).
        if let (Some(caller), true) = (caller, audited) {
            ctx.audit = Some(AuditState::default());
            ctx.audit_user_route = route.scope == Scope::User;
            let intent = audit_intent(session, ctx, matched, caller, request_id, &ip);
            if self.audit.write_intent(&intent).await.is_err() {
                return response::write_rejection(
                    &self.cors(),
                    session,
                    ctx,
                    route,
                    Rejection::Unavailable,
                )
                .await;
            }
        }

        if let Err(err) =
            upstream::build_and_store_upstream_peer(session, ctx, route, forwarding).await
        {
            let status = match err {
                upstream::UpstreamError::Internal => 500,
                upstream::UpstreamError::BadUpstream => 502,
            };
            return response::write_json_error(
                &self.cors(),
                session,
                ctx,
                status,
                ApiError::internal_error(),
            )
            .await;
        }
        Ok(false)
    }
}

impl Gateway {
    /// Route checks (docs/reference/gateway.md#route-checks): the credential as
    /// a route takes it, the body, one count of `default`, then each check
    /// through the pipeline's own match, admit and permission steps. Never
    /// forwarded, never audited.
    async fn handle_route_checks(
        &self,
        session: &mut Session,
        ctx: &mut GatewayContext,
        snapshot: &Snapshot,
    ) -> Result<bool> {
        let route = snapshot.route_checks.path.clone();
        if let Some(ref span) = ctx.root_span {
            span.record("http.route", route.as_str());
            span.context().span().update_name(format!("POST {route}"));
        }
        ctx.route = Some(route);
        ctx.max_body_bytes = MAX_CHECKS_BODY;
        let cors = snapshot.cors.clone();

        let presented = self
            .authenticator
            .present(session.req_header(), &snapshot.name_claims)
            .await;
        if let Ok(p) = &presented {
            ctx.credential.auth_type = Some(p.auth_type());
        }
        let credential = match GatewayPipeline::check_credential(presented) {
            Ok(c) => c,
            Err(rejection) => return write_check_rejection(&cors, session, ctx, rejection).await,
        };

        if let Some(done) = self.reject_oversized_content_length(session, ctx).await? {
            return Ok(done);
        }
        let mut body = Vec::new();
        while let Some(chunk) = session.read_request_body().await? {
            if body.len() as u64 + chunk.len() as u64 > MAX_CHECKS_BODY {
                warn!(
                    max_body_size = MAX_CHECKS_BODY,
                    "route checks body too large"
                );
                return response::write_json_error(
                    &cors,
                    session,
                    ctx,
                    413,
                    ApiError::payload_too_large(MAX_CHECKS_BODY),
                )
                .await;
            }
            body.extend_from_slice(&chunk);
        }
        let checks = match parse_checks(&body) {
            Ok(checks) => checks,
            Err(fields) => {
                let fields: Vec<(String, String)> =
                    fields.into_iter().map(|f| (f.path, f.message)).collect();
                return response::write_json_error(
                    &cors,
                    session,
                    ctx,
                    422,
                    ApiError::validation(&fields),
                )
                .await;
            }
        };

        let ip = client_ip(session, &self.trusted_proxies);
        let checked = self
            .pipeline
            .check_routes(
                &snapshot.routes,
                &snapshot.route_checks.default,
                &credential,
                &checks,
                &ip,
            )
            .await;
        let checked = match checked {
            Ok(c) => c,
            Err(rejection) => return write_check_rejection(&cors, session, ctx, rejection).await,
        };
        let answer = serde_json::json!({
            "checks": checked
                .allowed
                .iter()
                .map(|allowed| serde_json::json!({ "allowed": allowed }))
                .collect::<Vec<_>>()
        });
        let mut extras = Vec::new();
        if let Some(info) = &checked.rate_limit {
            extras.push(("X-RateLimit-Limit", info.limit.to_string()));
            extras.push(("X-RateLimit-Remaining", info.remaining.to_string()));
            extras.push(("X-RateLimit-Reset", info.reset_epoch.to_string()));
        }
        response::send_json(
            &cors,
            session,
            ctx,
            200,
            answer.to_string().into_bytes(),
            &extras,
        )
        .await?;
        Ok(true)
    }
}

/// A route checks call refused whole: its 401, 429 or 503.
async fn write_check_rejection(
    cors: &CorsPolicy,
    session: &mut Session,
    ctx: &GatewayContext,
    rejection: Rejection,
) -> Result<bool> {
    match rejection {
        Rejection::RateLimited { retry_after, info } => {
            response::write_rate_limited(cors, session, ctx, retry_after, info.as_ref()).await
        }
        Rejection::Unauthorized(err) => {
            response::write_json_error(
                cors,
                session,
                ctx,
                401,
                ApiError::unauthorized_any(err.details()),
            )
            .await
        }
        other => {
            response::write_json_error(
                cors,
                session,
                ctx,
                other.status(),
                ApiError::service_unavailable(),
            )
            .await
        }
    }
}

/// The request's trace, for a write that lands after the response.
fn request_context(ctx: &GatewayContext) -> opentelemetry::Context {
    ctx.root_span()
        .map(|span| span.context())
        .unwrap_or_default()
}

/// The client IP for audit and the IP limits ([`client_ip`]). A peer that is
/// not an inet socket is used as is.
fn client_ip(session: &Session, trusted: &TrustedProxies) -> String {
    let Some(addr) = session.client_addr() else {
        return "unknown".to_string();
    };
    let Some(sock) = addr.as_inet() else {
        return addr.to_string();
    };
    let forwarded_for = session
        .req_header()
        .headers
        .get_all("x-forwarded-for")
        .iter()
        .filter_map(|v| v.to_str().ok());
    let ip: IpAddr = trusted.client_ip(sock.ip(), forwarded_for);
    ip.to_string()
}

/// The intent for an audited request: the chain and who acted. Chain params'
/// values are slugs or ids as typed, so they stay out of `params`: their nodes are
/// in `nodes`.
fn audit_intent(
    session: &Session,
    ctx: &GatewayContext,
    matched: &Matched,
    caller: &Caller,
    request_id: &str,
    ip: &str,
) -> audit::Intent {
    audit::Intent {
        request_id: request_id.to_string(),
        occurred_at: audit::rfc3339_millis(ctx.received_at),
        nodes: caller.nodes.clone(),
        actor: audit::Actor {
            member_id: caller.member_id.clone(),
            user_id: caller.user_id.clone(),
            principal: caller.principal.clone(),
            auth_type: caller.auth_type.as_str(),
            key_id: caller.key_id.clone(),
        },
        service: matched.route.service.clone(),
        method: matched.route.method.clone(),
        route: matched.route.path.clone(),
        params: matched.params.clone(),
        ip: ip.to_string(),
        user_agent: session
            .req_header()
            .headers
            .get("user-agent")
            .map(|v| audit::user_agent(v.as_bytes())),
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

/// `[a,b]`: a permission list as one log field.
fn permission_list(permissions: &[String]) -> String {
    format!("[{}]", permissions.join(","))
}

/// The why of a permission-denied 403, next to the request line
/// (docs/reference/telemetry.md#gateway-permission-denied-line). Any one of
/// `required_permissions` would have admitted. `permissions` is the caller's on the
/// node; the gateway never sees roles or bindings. Log only: services and the 403
/// body never get the caller's permissions.
fn log_permission_denied(ctx: &GatewayContext, route: &Route, caller: &Caller) {
    let required = match &route.required {
        Required::AnyOf(list) => permission_list(list),
        Required::AnyMember => "any_member".to_string(),
        Required::Nothing => "[]".to_string(),
    };
    let cred = &ctx.credential;
    warn!(
        request_id = ctx.request_id.as_deref(),
        route = %route.path,
        method = %ctx.method_label(),
        status = 403u16,
        auth_type = cred.auth_type.map(|t| t.as_str()),
        key_id = cred.key_id.as_deref(),
        principal = cred.principal.as_deref(),
        root_id = cred.root_id.as_deref(),
        node_id = cred.node_id.as_deref(),
        member_id = cred.member_id.as_deref(),
        required_permissions = %required,
        permissions = %permission_list(&caller.permissions),
        "required_permissions denied"
    );
}

/// One line per request (docs/reference/telemetry.md#gateway-request-line). Credential
/// fields are present only when known: `key_id` once admit recognized a key, the ids
/// once admitted. Never the key or token itself, never a principal id.
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
    let key_id = credential.key_id.as_deref();
    let principal = credential.principal.as_deref();
    let root_id = credential.root_id.as_deref();
    let node_id = credential.node_id.as_deref();
    let member_id = credential.member_id.as_deref();
    let user_id = credential.user_id.as_deref();

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
                key_id,
                principal,
                root_id,
                node_id,
                member_id,
                user_id,
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
                key_id,
                principal,
                root_id,
                node_id,
                member_id,
                user_id,
                "request completed"
            );
        }
    }
}

#[async_trait]
impl ProxyHttp for Gateway {
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

        // Route checks: the gateway's own answer. No route matches the path.
        let snapshot = self.snapshot.load_full();
        if session.req_header().method == http::Method::POST && path == snapshot.route_checks.path {
            return self.handle_route_checks(session, ctx, &snapshot).await;
        }

        // Unmatched: 404, not counted by the failed-auth limiter.
        let Some((matched, name_claims)) = self.resolve_route(&path, &method) else {
            warn!("no matching route");
            return response::write_json_error(
                &self.cors(),
                session,
                ctx,
                404,
                ApiError::not_found(),
            )
            .await;
        };

        let route = &matched.route;
        ctx.route = Some(route.path.clone());
        ctx.max_body_bytes = route.max_body_bytes;
        if let Some(ref span) = ctx.root_span {
            span.record("http.route", route.path.as_str());
            span.context()
                .span()
                .update_name(format!("{} {}", method, route.path));
        }
        if let Some(done) = self.reject_oversized_content_length(session, ctx).await? {
            return Ok(done);
        }
        self.prepare_upstream(session, ctx, &matched, &name_claims, &request_id)
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
            if ctx.body_bytes > ctx.max_body_bytes {
                warn!(
                    body_bytes = ctx.body_bytes,
                    max_body_size = ctx.max_body_bytes,
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
                413 => ApiError::payload_too_large(ctx.max_body_bytes),
                502 => ApiError::service_unavailable(),
                _ => ApiError::internal_error(),
            };
            if let Err(err) =
                response::send_json_error(&self.cors(), session, ctx, code, error).await
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

        // A service's audit headers never reach the client, audited route or not.
        let header = |name: &str| {
            upstream_response
                .headers
                .get(name)
                .map(|v| v.as_bytes().to_vec())
        };
        let details = header(audit::DETAILS_HEADER);
        let created = header(audit::CREATED_HEADER);
        let named_node_ids = header(audit::NODES_HEADER);
        let named_member = header(audit::MEMBER_HEADER);
        let service_headers: Vec<_> = upstream_response
            .headers
            .keys()
            .filter(|name| name.as_str().starts_with(audit::SERVICE_HEADER_PREFIX))
            .cloned()
            .collect();
        for name in service_headers {
            upstream_response.remove_header(&name);
        }
        let route_label = ctx.route_label().to_string();
        let user_route = ctx.audit_user_route;
        if let Some(state) = ctx.audit.as_mut() {
            let status = upstream_response.status.as_u16();
            state.upstream_status = Some(status);
            if let Some(raw) = details {
                state.details = audit::parse_details(&raw);
                if state.details.is_none() {
                    warn!(
                        route = %route_label,
                        "ignoring audit details that are not one ASCII JSON object within 4096 bytes"
                    );
                }
            }
            if let Some(raw) = created {
                state.created_id = audit::parse_id(&raw);
                if state.created_id.is_none() {
                    warn!(route = %route_label, "ignoring an invalid audit created id");
                }
            }
            // Named nodes add logs only on a user route's success, and only as a pair.
            if named_node_ids.is_some() || named_member.is_some() {
                let pair = named_node_ids
                    .as_deref()
                    .and_then(audit::parse_nodes)
                    .zip(named_member.as_deref().and_then(audit::parse_id));
                if user_route && (200..300).contains(&status) && pair.is_some() {
                    state.named = pair;
                } else {
                    warn!(
                        route = %route_label,
                        status,
                        "ignoring audit nodes and member: not a pair on a user route's success"
                    );
                }
            }
        }
        self.cors()
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
            self.audit
                .send_outcome(request_id.to_string(), outcome, request_context(ctx));
        }

        ctx.finish_root_span();
    }
}
