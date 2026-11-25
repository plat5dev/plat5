mod context;
mod logging;
mod response;
mod route_checks;
pub mod signing;
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
use tracing::{debug, warn, Instrument};
use tracing_opentelemetry::OpenTelemetrySpanExt;
use uuid::Uuid;

use crate::audit::{self, Audit, AuditState};
use crate::auth::admit::{AdmitCache, HttpAdmit};
use crate::auth::invalidations::InvalidationTracker;
use crate::auth::jwt::JwtValidatorState;
use crate::auth::{AuthError, Authenticator, NameClaims};
use crate::authorizer::{AuthorizerCache, HttpAuthorizer};
use crate::client_ip::TrustedProxies;
use crate::config::GatewayConfig;
use crate::control_plane::Snapshot;
use crate::cors::CorsPolicy;
use crate::error::{ApiError, ErrorKind};
use crate::internal_http::InternalHttpClient;
use crate::metrics;
use crate::pipeline::{
    forward, Admitted, Caller, GatewayPipeline, Limit, Pipeline, Refused, Rejection,
};
use crate::rate_limit::RateLimiter;
use crate::route_map::{Matched, Scope};

pub use context::GatewayContext;

const ROUTE_CHECKS_PATH: &str = "/access/routes";

pub struct Gateway {
    authenticator: Authenticator,
    name_claims: NameClaims,
    pipeline: GatewayPipeline,
    /// Routes, rate limits and CORS from the control plane, swapped whole on each change.
    snapshot: Arc<ArcSwap<Snapshot>>,
    audit: Audit,
    trusted_proxies: TrustedProxies,
    upstream_signer: Option<signing::UpstreamSigner>,
    upstream_resolver: upstream::PeerResolver,
}

impl Gateway {
    pub fn new(
        cfg: &GatewayConfig,
        jwt_validator: JwtValidatorState,
        snapshot: Arc<ArcSwap<Snapshot>>,
        limiter: RateLimiter,
        audit: Audit,
        invalidations: InvalidationTracker,
    ) -> Self {
        let admit = HttpAdmit::new(&cfg.identity_internal_url, InternalHttpClient::new());
        let upstream_resolver = upstream::PeerResolver::new(cfg.upstream_block_private_ips);
        let authorizer = AuthorizerCache::new(HttpAuthorizer::new(upstream_resolver.clone()));
        Self {
            authenticator: Authenticator::new(
                jwt_validator,
                cfg.auth_subject_claim.clone(),
                cfg.auth_scope_claim.clone(),
            ),
            name_claims: cfg.auth_name_claims.clone(),
            pipeline: Pipeline::with_authorizer(
                AdmitCache::with_tracker(
                    admit,
                    cfg.admit_cache_ttl,
                    cfg.admit_negative_cache_ttl,
                    invalidations,
                ),
                limiter,
                Limit {
                    requests: cfg.rate_limit_auth_failure_requests,
                    window_seconds: cfg.rate_limit_auth_failure_window_seconds,
                },
                authorizer,
            ),
            snapshot,
            audit,
            trusted_proxies: cfg.trusted_proxies.clone(),
            upstream_signer: cfg.upstream_signer.clone(),
            upstream_resolver,
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

    async fn prepare_upstream(
        &self,
        session: &mut Session,
        ctx: &mut GatewayContext,
        snapshot: &Snapshot,
        matched: &Matched,
        request_id: &str,
    ) -> Result<bool> {
        let route = &matched.route;
        let cors = &snapshot.cors;
        if let Err(err) = session
            .req_header_mut()
            .insert_header("X-Request-ID", request_id)
        {
            warn!(
                error_type = ErrorKind::Internal.as_str(),
                error_message = %err,
                "failed to inject X-Request-ID header"
            );
            return response::write_json_error(cors, session, ctx, 500, ApiError::internal_error())
                .await;
        }

        let ip = client_ip(session, &self.trusted_proxies);
        let presented = match route.scope {
            // No credential is read on a public route.
            Scope::Public => Err(AuthError::MissingAuthorization),
            _ => {
                self.authenticator
                    .present(
                        session.req_header(),
                        &self.name_claims,
                        &snapshot.policy_claims,
                    )
                    .await
            }
        };
        let decided = self
            .pipeline
            .decide_with_authorizer(
                matched,
                &ip,
                presented,
                &mut ctx.credential,
                snapshot.authorizer.as_ref(),
                request_id,
            )
            .await;
        let audited = self.audit.enabled() && route.audit;
        let passed = match decided {
            Ok(passed) => passed,
            Err(Refused { rejection, caller }) => {
                if let Some(caller) = &caller {
                    upstream::record_admission_span(ctx, caller);
                }
                if let Some(caller) = &caller {
                    match &rejection {
                        Rejection::Denied => {
                            logging::log_permission_denied(ctx, route, caller);
                            if audited {
                                let intent =
                                    audit_intent(session, ctx, matched, caller, request_id, &ip);
                                self.audit.send_rejected(
                                    intent,
                                    rejection.status(),
                                    request_context(ctx),
                                );
                            }
                        }
                        Rejection::BadPathParam if audited => {
                            let intent =
                                audit_intent(session, ctx, matched, caller, request_id, &ip);
                            self.audit.send_rejected(
                                intent,
                                rejection.status(),
                                request_context(ctx),
                            );
                        }
                        _ => {}
                    }
                }
                return response::write_rejection(cors, session, ctx, route, rejection).await;
            }
        };
        ctx.rate_limit = passed.rate_limit;
        let admitted = passed.admitted;
        let caller = match &admitted {
            Admitted::Caller(c) => Some(c),
            Admitted::Public => None,
        };
        if let Some(caller) = caller {
            upstream::record_admission_span(ctx, caller);
        }

        let forwarding = match forward(matched, &admitted) {
            Ok(f) => f,
            Err(rejection) => {
                // A refusal after admit is one background event.
                if let (Some(caller), true) = (caller, audited) {
                    let intent = audit_intent(session, ctx, matched, caller, request_id, &ip);
                    self.audit
                        .send_rejected(intent, rejection.status(), request_context(ctx));
                }
                return response::write_rejection(cors, session, ctx, route, rejection).await;
            }
        };

        // Audited: in sync mode nothing reaches the service unless the intent
        // is written; in async mode the intent is held until the logging phase
        // sends one completed event (docs/concepts/audit.md#delivery).
        if let (Some(caller), true) = (caller, audited) {
            let intent = audit_intent(session, ctx, matched, caller, request_id, &ip);
            ctx.audit_user_route = route.scope == Scope::User;
            if self.audit.is_async() {
                ctx.audit = Some(AuditState {
                    occurred_at: Some(intent.occurred_at.clone()),
                    intent: Some(intent),
                    ..AuditState::default()
                });
            } else {
                // Arm outcome tracking BEFORE the intent write so if the intent
                // actually committed on audit before timing out here, logging()
                // resolves it to `rejected` (404 from audit if it never landed).
                ctx.audit = Some(AuditState {
                    occurred_at: Some(intent.occurred_at.clone()),
                    ..AuditState::default()
                });
                if self.audit.write_intent(&intent).await.is_err() {
                    return response::write_rejection(
                        cors,
                        session,
                        ctx,
                        route,
                        Rejection::Unavailable,
                    )
                    .await;
                }
            }
        }

        if let Err(err) = upstream::build_and_store_upstream_peer(
            session,
            ctx,
            route,
            forwarding,
            request_id,
            self.upstream_signer.as_ref(),
            &self.upstream_resolver,
        )
        .await
        {
            let (status, error) = match err {
                upstream::UpstreamError::Internal => (500, ApiError::internal_error()),
                upstream::UpstreamError::BadUpstream => (502, ApiError::service_unavailable()),
            };
            return response::write_json_error(cors, session, ctx, status, error).await;
        }
        Ok(false)
    }
}

/// The request's trace, for a write that lands after the response.
fn request_context(ctx: &GatewayContext) -> opentelemetry::Context {
    ctx.root_span()
        .map(|span| span.context())
        .unwrap_or_default()
}

/// The request's `Host` header (or HTTP/2 `:authority` URI host), for root host routing.
pub(super) fn request_host(req: &pingora::http::RequestHeader) -> Option<&str> {
    req.headers
        .get("host")
        .and_then(|v| v.to_str().ok())
        .or_else(|| req.uri.host())
}

/// The client IP for audit and the IP limits ([`client_ip`]). A peer that is
/// not an inet socket is used as is.
pub(super) fn client_ip(session: &Session, trusted: &TrustedProxies) -> String {
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

/// `url.scheme` for the request's span ([`TrustedProxies::url_scheme`]).
fn url_scheme(session: &Session, trusted: &TrustedProxies) -> &'static str {
    let peer = session
        .client_addr()
        .and_then(|addr| addr.as_inet())
        .map(|sock| sock.ip());
    let tls = session
        .digest()
        .is_some_and(|digest| digest.ssl_digest.is_some());
    let forwarded_proto = session
        .req_header()
        .headers
        .get_all("x-forwarded-proto")
        .iter()
        .filter_map(|v| v.to_str().ok());
    trusted.url_scheme(peer, tls, forwarded_proto)
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
        nodes: caller.nodes().to_vec(),
        actor: audit::Actor {
            member_id: caller.member_id().map(str::to_string),
            user_id: caller.user_id().map(str::to_string),
            principal: caller.principal.as_str().to_string(),
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
        let query = session.req_header().uri.query().map(str::to_string);
        let scheme = url_scheme(session, &self.trusted_proxies);
        let root_span = ctx.ensure_root_span(&method, scheme, &path, query.as_deref());
        let span = tracing::info_span!(parent: &root_span, "gateway.request_filter");

        async {
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
            if session.req_header().method == http::Method::POST && path == ROUTE_CHECKS_PATH {
                return self.handle_route_checks(session, ctx, &snapshot).await;
            }

            // Unmatched: 404, not counted by the failed-auth limiter.
            let host = request_host(session.req_header());
            let Some(matched) = snapshot.routes.find_route_with_host(&path, &method, host) else {
                warn!("no matching route");
                return response::write_json_error(
                    &snapshot.cors,
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
            self.prepare_upstream(session, ctx, &snapshot, &matched, &request_id)
                .await
        }
        .instrument(span)
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

        let error_type = logging::request_error_type(status, e.is_some());
        if let Some(ref span) = ctx.root_span {
            span.record("http.response.status_code", status);
            if let Some(kind) = error_type {
                span.record("error.type", kind);
                span.set_status(Status::error(""));
            } else if e.is_some() && !(400..500).contains(&status) {
                // A failure with no HTTP status to show for it, such as a client gone.
                span.set_status(Status::error(""));
            }
        }

        metrics::record_request(&route, &method, status, duration);

        let request_id = ctx.request_id.as_deref().unwrap_or("");
        let (trace_id, span_id) = logging::otel_trace_ids(root_span.as_ref());
        logging::log_request_outcome(
            logging::RequestLog {
                request_id,
                route: &route,
                method: &method,
                status,
                duration_ms: duration * 1000.0,
                trace_id: trace_id.as_deref(),
                span_id: span_id.as_deref(),
                credential: &ctx.credential,
                error_type,
            },
            e,
        );

        if let Some(mut state) = ctx.audit.take() {
            let intent = state.intent.take();
            let written = session.response_written().map(|r| r.status.as_u16());
            let outcome = audit::outcome_for(state, ctx.forwarded, written);
            if let Some(intent) = intent {
                self.audit
                    .send_completed(intent, outcome, request_context(ctx));
            } else {
                self.audit
                    .send_outcome(request_id.to_string(), outcome, request_context(ctx));
            }
        }

        ctx.finish_root_span();
    }
}
