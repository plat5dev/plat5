mod client_ip;
mod context;
mod cors;
mod response;
mod upstream;

use std::collections::HashMap;
use std::net::IpAddr;
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

use crate::audit::{self, Audit, AuditState};
use crate::auth::admit::{AdmitCache, HttpAdmit};
use crate::auth::jwt::JwtValidatorState;
use crate::auth::{AuthError, Authenticator};
use crate::config::GatewayConfig;
use crate::control_plane::Snapshot;
use crate::error::{ApiError, ErrorKind};
use crate::internal_http::InternalHttpClient;
use crate::metrics;
use crate::pipeline::{
    authorize, forward, Admitted, Caller, GatewayPipeline, Limit, Pipeline, Rejection,
    RequestCredential,
};
use crate::rate_limit::RateLimiter;
use crate::route_map::{Route, Scope};

pub use client_ip::TrustedProxies;
pub use context::GatewayContext;
pub use cors::CorsPolicy;

const MAX_BODY_SIZE_BYTES: u64 = 10 * 1024 * 1024; // 10 MB

pub struct Gateway {
    authenticator: Authenticator,
    pipeline: GatewayPipeline,
    /// Kinds, routes and CORS from the control plane, swapped whole on each change.
    snapshot: Arc<ArcSwap<Snapshot>>,
    connect_timeout: Duration,
    read_timeout: Duration,
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
        let admit = HttpAdmit::new(
            cfg.admit_url.clone(),
            InternalHttpClient::new(cfg.internal_auth_token.clone()),
        );
        Self {
            authenticator: Authenticator::new(jwt_validator, cfg.auth_subject_claim.clone()),
            pipeline: Pipeline::new(
                AdmitCache::new(admit, Duration::from_secs(cfg.apikey_cache_ttl_secs)),
                limiter,
                Limit {
                    requests: cfg.rate_limit_requests,
                    window_seconds: cfg.rate_limit_window_seconds,
                },
                Limit {
                    requests: cfg.rate_limit_auth_failure_requests,
                    window_seconds: cfg.rate_limit_auth_failure_window_seconds,
                },
            ),
            snapshot,
            connect_timeout: cfg.upstream_connect_timeout,
            read_timeout: cfg.upstream_read_timeout,
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
                &self.cors(),
                session,
                ctx,
                413,
                ApiError::payload_too_large(MAX_BODY_SIZE_BYTES),
            )
            .await?,
        ))
    }

    /// The matched route, its params, and the sign-in kind's name claim, from one snapshot.
    fn resolve_route(
        &self,
        path: &str,
        method: &str,
    ) -> Option<(Route, HashMap<String, String>, String)> {
        let snapshot = self.snapshot.load();
        snapshot
            .routes
            .find_route(path, method)
            .map(|(route, params)| {
                (
                    route.clone(),
                    params,
                    snapshot.kinds.name_claim().to_string(),
                )
            })
    }

    async fn prepare_upstream(
        &self,
        session: &mut Session,
        ctx: &mut GatewayContext,
        route: &Route,
        params: &HashMap<String, String>,
        name_claim: &str,
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
                    .present(session.req_header(), name_claim)
                    .await
            }
        };
        // request_filter has entered the root span: a JWT's kid is recorded on it.
        let method = ctx.method_label().to_string();
        let decided = self
            .pipeline
            .admit_and_limit(route, params, &method, &ip, presented, &mut ctx.credential)
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

        // Audited: the intent is written before anything past the limiter can answer,
        // and nothing reaches the service unless it is (docs/concepts/audit.md#delivery).
        if let Admitted::Caller(caller) = &admitted {
            if self.audit.enabled() && route.audited(ctx.method_label()) {
                ctx.audit = Some(AuditState::default());
                ctx.audit_sign_in_route = matches!(route.scope, Scope::SignIn(_));
                let intent = audit_intent(session, ctx, route, params, caller, request_id, &ip);
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
        }

        // After the limiter, so a label-denied request still spends budget.
        if let Err(rejection) = authorize(route, &admitted) {
            if let Admitted::Caller(caller) = &admitted {
                log_label_denied(ctx, route, caller);
            }
            return response::write_rejection(&self.cors(), session, ctx, route, rejection).await;
        }

        let forwarding = match forward(route, params, &admitted) {
            Ok(f) => f,
            Err(rejection) => {
                return response::write_rejection(&self.cors(), session, ctx, route, rejection)
                    .await
            }
        };
        if let Err(err) = upstream::build_and_store_upstream_peer(
            session,
            ctx,
            route,
            forwarding,
            route.timeouts.connect.unwrap_or(self.connect_timeout),
            route.timeouts.read.unwrap_or(self.read_timeout),
        )
        .await
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
    route: &Route,
    params: &HashMap<String, String>,
    caller: &Caller,
    request_id: &str,
    ip: &str,
) -> audit::Intent {
    audit::Intent {
        request_id: request_id.to_string(),
        occurred_at: audit::rfc3339_millis(ctx.received_at),
        root_id: caller.root_id().map(str::to_string),
        nodes: caller.nodes.clone(),
        actor: audit::Actor {
            member_id: caller.member_id.clone(),
            principal: caller.principal.clone(),
            auth_type: caller.auth_type.as_str(),
            key_prefix: caller.key_prefix.clone(),
        },
        service: route.service.clone(),
        method: ctx.method_label().to_string(),
        route: route.path.clone(),
        params: params
            .iter()
            .filter(|(name, _)| !route.is_chain_param(name))
            .map(|(k, v)| (k.clone(), v.clone()))
            .collect(),
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

/// `[a,b]`: a label list as one log field.
fn label_list(labels: &[String]) -> String {
    format!("[{}]", labels.join(","))
}

/// The why of a label-denied 403, next to the request line
/// (docs/reference/telemetry.md#gateway-label-denied-line). Any one of
/// `required_labels` would have admitted, so all of them are missing. `labels` is
/// the caller's on the node; the gateway never sees roles or bindings. Log only:
/// services and the 403 body never get the caller's labels.
fn log_label_denied(ctx: &GatewayContext, route: &Route, caller: &Caller) {
    let required = label_list(&route.required_labels);
    let labels = label_list(&caller.labels);
    let cred = &ctx.credential;
    warn!(
        request_id = ctx.request_id.as_deref(),
        route = %route.path,
        method = %ctx.method_label(),
        status = 403u16,
        auth_type = cred.auth_type.map(|t| t.as_str()),
        key_prefix = cred.key_prefix.as_deref(),
        principal = cred.principal.as_deref(),
        root_id = cred.root_id.as_deref(),
        node_id = cred.node_id.as_deref(),
        member_id = cred.member_id.as_deref(),
        required_labels = %required,
        missing_labels = %required,
        labels = %labels,
        "required_labels denied"
    );
}

/// One line per request (docs/reference/telemetry.md#gateway-request-line). Credential
/// fields are present only when known: `key_prefix` as soon as an `X-API-Key` of the
/// right shape is presented (so 401 and 403 carry it), the ids once admitted. Never
/// the key or token itself, never a person's principal id.
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
    let principal = credential.principal.as_deref();
    let root_id = credential.root_id.as_deref();
    let node_id = credential.node_id.as_deref();
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
                principal,
                root_id,
                node_id,
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
                principal,
                root_id,
                node_id,
                member_id,
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

        if let Some(done) = self.reject_oversized_content_length(session, ctx).await? {
            return Ok(done);
        }

        let Some((route, params, name_claim)) = self.resolve_route(&path, &method) else {
            warn!("no matching route");
            let ip = client_ip(session, &self.trusted_proxies);
            if let Some(limited) = self.pipeline.note_unadmitted(&ip).await {
                return match limited {
                    Rejection::RateLimited { retry_after, .. } => {
                        response::write_rate_limited(&self.cors(), session, ctx, retry_after, None)
                            .await
                    }
                    other => {
                        response::write_json_error(
                            &self.cors(),
                            session,
                            ctx,
                            other.status(),
                            ApiError::service_unavailable(),
                        )
                        .await
                    }
                };
            }
            return response::write_json_error(
                &self.cors(),
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
        self.prepare_upstream(session, ctx, &route, &params, &name_claim, &request_id)
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
        let named_root = header(audit::ROOT_HEADER);
        let named_member = header(audit::MEMBER_HEADER);
        for name in audit::SERVICE_HEADERS {
            upstream_response.remove_header(*name);
        }
        let route_label = ctx.route_label().to_string();
        let sign_in_route = ctx.audit_sign_in_route;
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
            // The root pair adds a log only on a sign-in route's success, and only as a pair.
            if named_root.is_some() || named_member.is_some() {
                let pair = named_root
                    .as_deref()
                    .and_then(audit::parse_id)
                    .zip(named_member.as_deref().and_then(audit::parse_id));
                if sign_in_route && (200..300).contains(&status) && pair.is_some() {
                    state.named = pair;
                } else {
                    warn!(
                        route = %route_label,
                        status,
                        "ignoring audit root and member: not a pair on a sign-in route's success"
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
            let parent = ctx
                .root_span()
                .map(|span| span.context())
                .unwrap_or_default();
            self.audit
                .send_outcome(request_id.to_string(), outcome, parent);
        }

        ctx.finish_root_span();
    }
}
