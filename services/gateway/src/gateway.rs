use std::collections::HashMap;
use std::sync::Arc;
use std::time::Instant;

use arc_swap::ArcSwap;
use async_trait::async_trait;
use bytes::Bytes;
use http::header::HeaderName;
use http::HeaderValue;
use opentelemetry::global;
use opentelemetry::propagation::Injector;
use opentelemetry::trace::{Status, TraceContextExt};
use pingora::http::{RequestHeader, ResponseHeader};
use pingora::proxy::{FailToProxy, ProxyHttp, Session};
use pingora::upstreams::peer::HttpPeer;
use pingora::{Error, ErrorSource, ErrorType, Result};
use tracing::info;
use tracing::{debug, warn, Span};
use tracing_opentelemetry::OpenTelemetrySpanExt;
use uuid::Uuid;

use crate::admission::{
    extract_claim_path, jwt_error_reason, organization_id_from_params, AuthContext, AuthError,
    OrgParamError, ResolveDeny,
};
use crate::apikey_cache::ApiKeyCache;
use crate::auth::apikey::{
    ApiKeyError, MemberApiKeyValidator, UserApiKeyValidator, MEMBER_KEY_PREFIX, USER_KEY_PREFIX,
};
use crate::auth::membership::{MembershipError, MembershipResolver};
use crate::member_apikey_cache::MemberApiKeyCache;
use crate::auth::{jwt::validate_token, state::JwtValidatorState};
use crate::error::{ApiError, ErrorKind};
use crate::jwt_cache::JwtCache;
use crate::membership_cache::MembershipCache;
use crate::metrics;
use crate::route_map::{Route, RouteMap, RouteScope};

pub use crate::admission::parse_user_id_claim;

const JWT_CACHE_CAPACITY: u64 = 10_000;
const JWT_CACHE_TTL_BUFFER_SECS: u64 = 60;
const APIKEY_CACHE_CAPACITY: u64 = 10_000;
const APIKEY_CACHE_DEFAULT_TTL_SECS: u64 = 300;
const MEMBERSHIP_CACHE_CAPACITY: u64 = 10_000;
const MEMBERSHIP_CACHE_DEFAULT_TTL_SECS: u64 = 300;
const MAX_BODY_SIZE_BYTES: u64 = 10 * 1024 * 1024; // 10 MB

const IDENTITY_HEADERS: &[&str] = &["X-User-Id", "X-Organization-Id", "X-Member-Id"];
const CLIENT_CREDENTIAL_HEADERS: &[&str] = &["Authorization", "X-API-Key"];

pub struct UserGateway {
    jwt_validator: JwtValidatorState,
    jwt_cache: JwtCache,
    /// Dotted JSON path into JWT claims for the Plat5 user id (e.g. `properties.user_id`, `sub`).
    user_id_claim: Vec<String>,
    user_apikey_validator: UserApiKeyValidator,
    apikey_cache: ApiKeyCache,
    member_apikey_validator: Option<MemberApiKeyValidator>,
    member_apikey_cache: MemberApiKeyCache,
    membership_resolver: Option<MembershipResolver>,
    membership_cache: MembershipCache,
    route_map: Arc<ArcSwap<RouteMap>>,
    connect_timeout: std::time::Duration,
    read_timeout: std::time::Duration,
    /// Empty = reflect `*`. Non-empty = allowlist; matched Origin is reflected with `Vary: Origin`.
    allowed_origins: Vec<String>,
}

impl UserGateway {
    #[allow(clippy::too_many_arguments)]
    pub fn new(
        jwt_validator: JwtValidatorState,
        user_id_claim: Vec<String>,
        user_apikey_validator: UserApiKeyValidator,
        apikey_cache_ttl_secs: Option<u64>,
        member_apikey_validator: Option<MemberApiKeyValidator>,
        membership_resolver: Option<MembershipResolver>,
        membership_cache_ttl_secs: Option<u64>,
        route_map: Arc<ArcSwap<RouteMap>>,
        connect_timeout: std::time::Duration,
        read_timeout: std::time::Duration,
        allowed_origins: Vec<String>,
    ) -> Self {
        let ttl = apikey_cache_ttl_secs.unwrap_or(APIKEY_CACHE_DEFAULT_TTL_SECS);
        let membership_ttl = membership_cache_ttl_secs.unwrap_or(MEMBERSHIP_CACHE_DEFAULT_TTL_SECS);
        Self {
            jwt_validator,
            jwt_cache: JwtCache::new(JWT_CACHE_CAPACITY, JWT_CACHE_TTL_BUFFER_SECS),
            user_id_claim,
            user_apikey_validator,
            apikey_cache: ApiKeyCache::new(APIKEY_CACHE_CAPACITY, ttl),
            member_apikey_validator,
            member_apikey_cache: MemberApiKeyCache::new(APIKEY_CACHE_CAPACITY, ttl),
            membership_resolver,
            membership_cache: MembershipCache::new(MEMBERSHIP_CACHE_CAPACITY, membership_ttl),
            route_map,
            connect_timeout,
            read_timeout,
            allowed_origins,
        }
    }

    fn strip_identity_headers(req: &mut RequestHeader) {
        for name in IDENTITY_HEADERS {
            req.remove_header(*name);
        }
    }

    fn strip_client_credentials(req: &mut RequestHeader) {
        for name in CLIENT_CREDENTIAL_HEADERS {
            req.remove_header(*name);
        }
    }

    fn apply_cors(&self, header: &mut ResponseHeader, request_origin: Option<&str>) -> Result<()> {
        if self.allowed_origins.is_empty() {
            header.insert_header("Access-Control-Allow-Origin", "*")?;
        } else if let Some(origin) = request_origin {
            if self.allowed_origins.iter().any(|allowed| allowed == origin) {
                header.insert_header("Access-Control-Allow-Origin", origin)?;
                header.insert_header("Vary", "Origin")?;
            }
        }
        header.insert_header(
            "Access-Control-Allow-Methods",
            "GET, POST, PUT, PATCH, DELETE, OPTIONS",
        )?;
        header.insert_header(
            "Access-Control-Allow-Headers",
            "Content-Type, Authorization, X-API-Key",
        )?;
        Ok(())
    }

    /// Client errors → span Ok; infrastructure failures → span Error.
    fn apply_error_span_status(span: &Span, status: u16) {
        match status {
            500 => {
                span.record("error.kind", "internal");
                span.set_status(Status::error("internal server error"));
            }
            503 => {
                span.record("error.kind", "network");
                span.set_status(Status::error("service unavailable"));
            }
            400 | 401 | 404 | 413 => {
                span.set_status(Status::Ok);
            }
            _ => {}
        }
    }

    /// Write a standardized Plat5 JSON error response.
    async fn send_json_error(
        &self,
        session: &mut Session,
        ctx: &GatewayContext,
        status: u16,
        error: ApiError,
    ) -> Result<()> {
        let body = error.to_json_bytes(ctx.request_id.as_deref());

        if let Some(ref span) = ctx.root_span {
            Self::apply_error_span_status(span, status);
        }

        let mut header = ResponseHeader::build(status, None)?;
        header.insert_header("Content-Type", "application/json")?;
        header.insert_header("Content-Length", body.len().to_string())?;
        self.apply_cors(&mut header, ctx.request_origin.as_deref())?;
        if let Some(ref request_id) = ctx.request_id {
            header.insert_header("X-Request-ID", request_id)?;
        }
        session
            .write_response_header(Box::new(header), false)
            .await?;
        session
            .write_response_body(Some(Bytes::from(body)), true)
            .await?;
        Ok(())
    }

    /// Write a standardized Plat5 JSON error response and short-circuit.
    async fn write_json_error(
        &self,
        session: &mut Session,
        ctx: &GatewayContext,
        status: u16,
        error: ApiError,
    ) -> Result<bool> {
        self.send_json_error(session, ctx, status, error).await?;
        Ok(true)
    }

    /// Check for JWT authentication via Authorization: Bearer header
    async fn check_jwt(&self, req: &RequestHeader) -> Result<AuthContext, AuthError> {
        let authorization = req
            .headers
            .get("Authorization")
            .ok_or(AuthError::MissingAuthorization)?;

        let auth_value = authorization
            .to_str()
            .map_err(|_| AuthError::InvalidAuthorizationHeader)?;
        let mut parts = auth_value.split_whitespace();
        match (parts.next(), parts.next()) {
            (Some("Bearer"), Some(token)) => {
                if let Some(cached_claims) = self.jwt_cache.get(token).await {
                    let user_id = extract_claim_path(&cached_claims.claims, &self.user_id_claim)
                        .ok_or(AuthError::MissingUserId)?;
                    return Ok(AuthContext {
                        user_id,
                        auth_type: "jwt",
                        kid: cached_claims.header.kid.clone(),
                    });
                }

                let jwks = self
                    .jwt_validator
                    .get_jwks()
                    .await
                    .map_err(|_| AuthError::JwtValidationUnavailable)?;
                let (claims, kid) = validate_token(
                    token,
                    self.jwt_validator.get_issuer(),
                    jwks,
                    self.jwt_validator.get_allowed_audiences().to_vec(),
                )
                .await
                .map_err(|e| AuthError::InvalidToken {
                    reason: jwt_error_reason(&e),
                })?;

                self.jwt_cache.put(token, claims.clone()).await;

                let user_id = extract_claim_path(&claims.claims, &self.user_id_claim)
                    .ok_or(AuthError::MissingUserId)?;
                Ok(AuthContext {
                    user_id,
                    auth_type: "jwt",
                    kid: Some(kid),
                })
            }
            _ => Err(AuthError::InvalidAuthorizationHeader),
        }
    }

    /// User API key auth via X-API-Key (`plat5-sk-1-…` only).
    /// Member keys (`plat5-mk-1-…`) are organization-scope only — not accepted here.
    async fn check_api_key(&self, req: &RequestHeader) -> Result<AuthContext, AuthError> {
        let api_key = req
            .headers
            .get("X-API-Key")
            .ok_or(AuthError::MissingApiKey)?;

        let key = api_key
            .to_str()
            .map_err(|_| AuthError::InvalidApiKeyHeader)?;

        if !key.starts_with(USER_KEY_PREFIX) {
            return Err(AuthError::InvalidApiKey);
        }

        if let Some(cached) = self.apikey_cache.get(key).await {
            return Ok(AuthContext {
                user_id: cached.user_id,
                auth_type: "apikey",
                kid: None,
            });
        }

        let validation = self
            .user_apikey_validator
            .validate(key)
            .await
            .map_err(|e| match e {
                ApiKeyError::InvalidKey => AuthError::InvalidApiKey,
                ApiKeyError::ServiceError(msg) => {
                    warn!(
                        error_kind = ErrorKind::Network.as_str(),
                        error_message = %msg,
                        "user key validate error"
                    );
                    AuthError::ApiKeyValidationUnavailable
                }
            })?;

        let user_id = validation.user_id.clone().ok_or_else(|| {
            warn!("user key validate returned valid without user_id");
            AuthError::ApiKeyValidationUnavailable
        })?;

        self.apikey_cache.put(key, user_id.clone()).await;

        Ok(AuthContext {
            user_id,
            auth_type: "apikey",
            kid: None,
        })
    }

    /// Authenticate the request using either API key or JWT
    async fn authenticate(&self, req: &RequestHeader) -> Result<AuthContext, AuthError> {
        if req.headers.contains_key("X-API-Key") {
            return self.check_api_key(req).await;
        }
        self.check_jwt(req).await
    }

    async fn handle_user_scope(
        &self,
        session: &mut Session,
        ctx: &mut GatewayContext,
        path: &str,
        route: &Route,
        params: &HashMap<String, String>,
    ) -> Result<bool> {
        match self.authenticate(session.req_header()).await {
            Ok(auth_ctx) => {
                if let Err(err) = session
                    .req_header_mut()
                    .insert_header("X-User-Id", &auth_ctx.user_id)
                {
                    warn!(
                        error_kind = ErrorKind::Internal.as_str(),
                        error_message = %err,
                        "failed to inject X-User-Id header"
                    );
                    return self
                        .write_json_error(session, ctx, 500, ApiError::internal_error())
                        .await;
                }

                if let Some(ref span) = ctx.root_span {
                    span.record("user.id", auth_ctx.user_id.as_str());
                    if let Some(ref kid) = auth_ctx.kid {
                        span.record("jwt.kid", kid.as_str());
                    }
                }

                debug!(
                    auth_type = auth_ctx.auth_type,
                    user_id = %auth_ctx.user_id,
                    "authentication successful"
                );

                self.build_and_store_upstream_peer(session, ctx, path, route, params)?;
                Ok(false)
            }
            Err(err) => self.write_auth_error(session, ctx, err).await,
        }
    }

    async fn handle_organization_scope(
        &self,
        session: &mut Session,
        ctx: &mut GatewayContext,
        path: &str,
        route: &Route,
        params: &HashMap<String, String>,
    ) -> Result<bool> {
        let organization_id =
            match organization_id_from_params(route.organization_param.as_deref(), params) {
                Ok(id) => id,
                Err(OrgParamError::MissingParamName) => {
                    warn!("organization route missing organization_param (config bug)");
                    return self
                        .write_json_error(session, ctx, 500, ApiError::internal_error())
                        .await;
                }
                Err(OrgParamError::MissingParamValue { param }) => {
                    warn!(
                        param = %param,
                        "organization id missing from path params (config bug)"
                    );
                    return self
                        .write_json_error(session, ctx, 500, ApiError::internal_error())
                        .await;
                }
            };

        if let Some(ref span) = ctx.root_span {
            span.record("organization.id", organization_id.as_str());
        }

        // Member API key (plat5-mk-1-): validate → org match → inject. No member resolve.
        let member_key = session
            .req_header()
            .headers
            .get("X-API-Key")
            .and_then(|v| v.to_str().ok())
            .filter(|k| k.starts_with(MEMBER_KEY_PREFIX))
            .map(|s| s.to_string());
        if let Some(key_str) = member_key {
            return self
                .admit_member_api_key(
                    session,
                    ctx,
                    path,
                    route,
                    params,
                    &organization_id,
                    &key_str,
                )
                .await;
        }

        // User credential (JWT or user API key) → member resolve.
        let auth_ctx = match self.authenticate(session.req_header()).await {
            Ok(a) => a,
            Err(err) => return self.write_auth_error(session, ctx, err).await,
        };

        if let Some(ref span) = ctx.root_span {
            span.record("user.id", auth_ctx.user_id.as_str());
            if let Some(ref kid) = auth_ctx.kid {
                span.record("jwt.kid", kid.as_str());
            }
        }

        let member_id = match self
            .resolve_active_membership(&auth_ctx.user_id, &organization_id)
            .await
        {
            Ok(id) => id,
            Err(ResolveDeny::NotFound) => {
                debug!(
                    user_id = %auth_ctx.user_id,
                    organization_id = %organization_id,
                    "member resolve miss or inactive"
                );
                return self
                    .write_json_error(session, ctx, 404, ApiError::not_found())
                    .await;
            }
            Err(ResolveDeny::Unavailable) => {
                warn!(
                    user_id = %auth_ctx.user_id,
                    organization_id = %organization_id,
                    "member resolve unavailable"
                );
                return self
                    .write_json_error(session, ctx, 503, ApiError::service_unavailable())
                    .await;
            }
        };

        self.inject_org_headers_and_forward(
            session,
            ctx,
            path,
            route,
            params,
            &organization_id,
            &member_id,
            auth_ctx.auth_type,
            Some(auth_ctx.user_id.as_str()),
        )
        .await
    }

    async fn admit_member_api_key(
        &self,
        session: &mut Session,
        ctx: &mut GatewayContext,
        path: &str,
        route: &Route,
        params: &HashMap<String, String>,
        path_organization_id: &str,
        key: &str,
    ) -> Result<bool> {
        if let Some(cached) = self.member_apikey_cache.get(key).await {
            if cached.organization_id != path_organization_id {
                debug!(
                    key_org = %cached.organization_id,
                    path_org = %path_organization_id,
                    "member key org mismatch"
                );
                return self
                    .write_json_error(session, ctx, 404, ApiError::not_found())
                    .await;
            }
            return self
                .inject_org_headers_and_forward(
                    session,
                    ctx,
                    path,
                    route,
                    params,
                    path_organization_id,
                    &cached.member_id,
                    "member_apikey",
                    None,
                )
                .await;
        }

        let validator = match self.member_apikey_validator.as_ref() {
            Some(v) => v,
            None => {
                warn!("member api key presented but MEMBER_APIKEY_VALIDATE_URL unset");
                return self
                    .write_json_error(session, ctx, 503, ApiError::service_unavailable())
                    .await;
            }
        };

        let validation = match validator.validate(key).await {
            Ok(v) => v,
            Err(ApiKeyError::InvalidKey) => {
                metrics::record_auth_failure("member_apikey", "invalid_apikey");
                return self.write_auth_error(session, ctx, AuthError::InvalidApiKey).await;
            }
            Err(ApiKeyError::ServiceError(msg)) => {
                warn!(
                    error_kind = ErrorKind::Network.as_str(),
                    error_message = %msg,
                    "member key validate error"
                );
                return self
                    .write_json_error(session, ctx, 503, ApiError::service_unavailable())
                    .await;
            }
        };

        let member_id = match validation.member_id.clone() {
            Some(id) if !id.is_empty() => id,
            _ => {
                warn!("member key validate returned valid without member_id");
                return self
                    .write_json_error(session, ctx, 503, ApiError::service_unavailable())
                    .await;
            }
        };
        let key_org = match validation.organization_id.clone() {
            Some(id) if !id.is_empty() => id,
            _ => {
                warn!("member key validate returned valid without organization_id");
                return self
                    .write_json_error(session, ctx, 503, ApiError::service_unavailable())
                    .await;
            }
        };

        if key_org != path_organization_id {
            debug!(
                key_org = %key_org,
                path_org = %path_organization_id,
                "member key org mismatch"
            );
            return self
                .write_json_error(session, ctx, 404, ApiError::not_found())
                .await;
        }

        self.member_apikey_cache
            .put(key, member_id.clone(), key_org)
            .await;

        self.inject_org_headers_and_forward(
            session,
            ctx,
            path,
            route,
            params,
            path_organization_id,
            &member_id,
            "member_apikey",
            None,
        )
        .await
    }

    #[allow(clippy::too_many_arguments)]
    async fn inject_org_headers_and_forward(
        &self,
        session: &mut Session,
        ctx: &mut GatewayContext,
        path: &str,
        route: &Route,
        params: &HashMap<String, String>,
        organization_id: &str,
        member_id: &str,
        auth_type: &str,
        user_id: Option<&str>,
    ) -> Result<bool> {
        if let Some(ref span) = ctx.root_span {
            span.record("member.id", member_id);
        }

        // Org scope: only org + member headers — never X-User-Id
        if let Err(err) = session
            .req_header_mut()
            .insert_header("X-Organization-Id", organization_id)
        {
            warn!(
                error_kind = ErrorKind::Internal.as_str(),
                error_message = %err,
                "failed to inject X-Organization-Id header"
            );
            return self
                .write_json_error(session, ctx, 500, ApiError::internal_error())
                .await;
        }
        if let Err(err) = session
            .req_header_mut()
            .insert_header("X-Member-Id", member_id)
        {
            warn!(
                error_kind = ErrorKind::Internal.as_str(),
                error_message = %err,
                "failed to inject X-Member-Id header"
            );
            return self
                .write_json_error(session, ctx, 500, ApiError::internal_error())
                .await;
        }

        match user_id {
            Some(uid) => debug!(
                auth_type,
                user_id = %uid,
                organization_id = %organization_id,
                member_id = %member_id,
                "organization admission successful"
            ),
            None => debug!(
                auth_type,
                organization_id = %organization_id,
                member_id = %member_id,
                "organization admission successful"
            ),
        }

        self.build_and_store_upstream_peer(session, ctx, path, route, params)?;
        Ok(false)
    }

    async fn resolve_active_membership(
        &self,
        user_id: &str,
        organization_id: &str,
    ) -> Result<String, ResolveDeny> {
        if let Some(cached) = self.membership_cache.get(user_id, organization_id).await {
            return Ok(cached.member_id);
        }

        let resolver = self
            .membership_resolver
            .as_ref()
            .ok_or(ResolveDeny::Unavailable)?;

        match resolver.resolve(user_id, organization_id).await {
            Ok(resolved) => {
                if resolved.status != "active" {
                    return Err(ResolveDeny::NotFound);
                }
                let member_id = resolved.member_id;
                self.membership_cache
                    .put(user_id, organization_id, member_id.clone())
                    .await;
                Ok(member_id)
            }
            Err(MembershipError::NotFound) => Err(ResolveDeny::NotFound),
            Err(MembershipError::ServiceError(_)) => Err(ResolveDeny::Unavailable),
        }
    }

    async fn write_auth_error(
        &self,
        session: &mut Session,
        ctx: &GatewayContext,
        err: AuthError,
    ) -> Result<bool> {
        if err.is_client_error() {
            metrics::record_auth_failure(err.auth_type(), err.as_str());
            info!(
                error_kind = ErrorKind::Auth.as_str(),
                auth_type = err.auth_type(),
                error_message = err.as_str(),
                "authentication failed"
            );
            self.write_json_error(session, ctx, 401, ApiError::unauthorized(err.details()))
                .await
        } else {
            warn!(
                error_kind = ErrorKind::Network.as_str(),
                auth_type = err.auth_type(),
                error_message = err.as_str(),
                "authentication infrastructure unavailable"
            );
            self.write_json_error(session, ctx, 503, ApiError::service_unavailable())
                .await
        }
    }

    /// Build the upstream peer for a matched route and store it in context.
    fn build_and_store_upstream_peer(
        &self,
        session: &mut Session,
        ctx: &mut GatewayContext,
        original_path: &str,
        route: &Route,
        params: &HashMap<String, String>,
    ) -> Result<()> {
        Self::strip_client_credentials(session.req_header_mut());

        let upstream_path = route.resolve_upstream_path(params);
        if upstream_path != original_path {
            match http::Uri::builder()
                .path_and_query(upstream_path.as_str())
                .build()
            {
                Ok(new_uri) => {
                    session.req_header_mut().set_uri(new_uri);
                    debug!(
                        original = %original_path,
                        transformed = %upstream_path,
                        "path transformed"
                    );
                }
                Err(e) => {
                    warn!(
                        error_kind = ErrorKind::Internal.as_str(),
                        error_message = %e,
                        "failed to build transformed URI"
                    );
                    return Err(Error::new(ErrorType::HTTPStatus(500)));
                }
            }
        }

        {
            let mut injector = RequestHeaderInjector {
                headers: session.req_header_mut(),
            };
            let cx = ctx
                .root_span()
                .map(|span| span.context())
                .unwrap_or_else(|| tracing::Span::current().context());
            global::get_text_map_propagator(|propagator| {
                propagator.inject_context(&cx, &mut injector)
            });
        }

        let peer_addr = peer_address(&route.base_url);
        info!(route = %route.path, upstream = %peer_addr, "forwarding request to upstream");
        let mut peer = HttpPeer::new(peer_addr, false, String::new());
        peer.options.connection_timeout = Some(self.connect_timeout);
        peer.options.read_timeout = Some(self.read_timeout);
        ctx.upstream_peer = Some(Box::new(peer));
        Ok(())
    }
}

/// Gateway peers are host:port. Strip accidental scheme/path from route config.
fn peer_address(base_url: &str) -> String {
    let s = base_url.trim();
    let without_scheme = s
        .strip_prefix("https://")
        .or_else(|| s.strip_prefix("http://"))
        .unwrap_or(s);
    without_scheme
        .split('/')
        .next()
        .unwrap_or(without_scheme)
        .to_string()
}

pub struct GatewayContext {
    start: Instant,
    route: Option<String>,
    method: Option<String>,
    request_id: Option<String>,
    request_origin: Option<String>,
    root_span: Option<Span>,
    upstream_peer: Option<Box<HttpPeer>>,
    body_bytes: u64,
}

impl GatewayContext {
    fn new() -> Self {
        Self {
            start: Instant::now(),
            route: None,
            method: None,
            request_id: None,
            request_origin: None,
            root_span: None,
            upstream_peer: None,
            body_bytes: 0,
        }
    }

    fn route_label(&self) -> &str {
        self.route.as_deref().unwrap_or("unmatched")
    }

    fn method_label(&self) -> &str {
        self.method.as_deref().unwrap_or("UNKNOWN")
    }

    fn ensure_root_span(&mut self, path: &str, method: &str) -> Span {
        if let Some(ref span) = self.root_span {
            return span.clone();
        }

        let span = tracing::info_span!(
            "gateway.request",
            http.route = %path,
            http.method = %method,
            user.id = tracing::field::Empty,
            organization.id = tracing::field::Empty,
            member.id = tracing::field::Empty,
            request_id = tracing::field::Empty,
            http.status_code = tracing::field::Empty,
            jwt.kid = tracing::field::Empty,
            error.kind = tracing::field::Empty,
        );
        self.root_span = Some(span.clone());
        span
    }

    fn root_span(&self) -> Option<Span> {
        self.root_span.as_ref().cloned()
    }

    fn finish_root_span(&mut self) {
        self.root_span = None;
    }
}

fn otel_trace_ids(span: Option<&Span>) -> (Option<String>, Option<String>) {
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

struct RequestHeaderInjector<'a> {
    headers: &'a mut RequestHeader,
}

impl<'a> Injector for RequestHeaderInjector<'a> {
    fn set(&mut self, key: &str, value: String) {
        let header_name = match HeaderName::from_bytes(key.as_bytes()) {
            Ok(name) => name,
            Err(err) => {
                warn!(key = %key, ?err, "failed to parse propagation header name");
                return;
            }
        };
        let header_value = match HeaderValue::from_str(&value) {
            Ok(val) => val,
            Err(err) => {
                warn!(key = %key, ?err, "failed to parse propagation header value");
                return;
            }
        };
        if let Err(err) = self.headers.insert_header(header_name, header_value) {
            warn!(key = %key, ?err, "failed to inject propagation header");
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
            self.apply_cors(&mut header, ctx.request_origin.as_deref())?;
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
                        return self
                            .write_json_error(
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
                    return self
                        .write_json_error(session, ctx, 404, ApiError::not_found())
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
            return self
                .write_json_error(session, ctx, 500, ApiError::internal_error())
                .await;
        }

        Self::strip_identity_headers(session.req_header_mut());

        match route.scope {
            RouteScope::Public => {
                debug!(path = %path, "skipping auth for public route");
                self.build_and_store_upstream_peer(session, ctx, &path, &route, &params)?;
                Ok(false)
            }
            RouteScope::User => {
                self.handle_user_scope(session, ctx, &path, &route, &params)
                    .await
            }
            RouteScope::Organization => {
                self.handle_organization_scope(session, ctx, &path, &route, &params)
                    .await
            }
        }
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
                // fail_to_proxy writes the Plat5 JSON envelope
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
            if let Err(err) = self.send_json_error(session, ctx, code, error).await {
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
        self.apply_cors(upstream_response, ctx.request_origin.as_deref())?;

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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn strip_client_credentials_removes_auth_material() {
        let mut req = RequestHeader::build("GET", b"/api/x", None).unwrap();
        req.insert_header("Authorization", "Bearer secret-jwt")
            .unwrap();
        req.insert_header("X-API-Key", "plat5-sk-1-test").unwrap();
        req.insert_header("X-User-Id", "user-1").unwrap();
        req.insert_header("X-Request-ID", "req-1").unwrap();

        UserGateway::strip_client_credentials(&mut req);

        assert!(req.headers.get("Authorization").is_none());
        assert!(req.headers.get("X-API-Key").is_none());
        assert_eq!(
            req.headers.get("X-User-Id").and_then(|v| v.to_str().ok()),
            Some("user-1")
        );
        assert_eq!(
            req.headers
                .get("X-Request-ID")
                .and_then(|v| v.to_str().ok()),
            Some("req-1")
        );
    }

    #[test]
    fn strip_client_credentials_is_idempotent_when_absent() {
        let mut req = RequestHeader::build("GET", b"/public", None).unwrap();
        req.insert_header("X-Request-ID", "req-2").unwrap();
        UserGateway::strip_client_credentials(&mut req);
        assert!(req.headers.get("Authorization").is_none());
        assert!(req.headers.get("X-API-Key").is_none());
        assert_eq!(
            req.headers
                .get("X-Request-ID")
                .and_then(|v| v.to_str().ok()),
            Some("req-2")
        );
    }
}
