use async_trait::async_trait;
use http::Response;
use pingora::apps::http_app::{HttpServer, ServeHttp};
use pingora::modules::http::compression::ResponseCompressionBuilder;
use pingora::protocols::http::ServerSession;
use prometheus::{Encoder, TextEncoder};
use std::sync::Arc;
use std::time::Instant;

use crate::auth::jwt::JwtValidatorState;
use crate::authorizer::{AuthorizerInvalidator, InvalidateRequest};
use crate::rate_limit::RateLimiter;

const MAX_INVALIDATE_BODY: usize = 64 * 1024;

/// Shared state for health checks and internal endpoints.
pub struct HealthState {
    start_time: Instant,
    jwt_validator: JwtValidatorState,
    limiter: RateLimiter,
    authorizer_invalidator: AuthorizerInvalidator,
}

impl HealthState {
    pub fn new(
        jwt_validator: JwtValidatorState,
        limiter: RateLimiter,
        authorizer_invalidator: AuthorizerInvalidator,
    ) -> Self {
        Self {
            start_time: Instant::now(),
            jwt_validator,
            limiter,
            authorizer_invalidator,
        }
    }
}

/// HTTP app that serves /health/live, /health/ready, and /metrics endpoints
pub struct HealthHttpApp {
    state: Arc<HealthState>,
}

impl HealthHttpApp {
    pub fn new(state: Arc<HealthState>) -> Self {
        Self { state }
    }
}

#[async_trait]
impl ServeHttp for HealthHttpApp {
    async fn response(&self, http_session: &mut ServerSession) -> Response<Vec<u8>> {
        let method = http_session.req_header().method.clone();
        let path = http_session.req_header().uri.path().to_string();

        match (method.as_str(), path.as_str()) {
            ("GET", "/health/live") => self.live_response(),
            ("GET", "/health/ready") => self.ready_response().await,
            ("GET", "/metrics") => self.metrics_response(),
            ("POST", "/internal/authorizer-cache/invalidate") => {
                self.invalidate_authorizer_cache(http_session).await
            }
            _ => Response::builder()
                .status(404)
                .header(http::header::CONTENT_TYPE, "text/plain")
                .header(http::header::CONTENT_LENGTH, "Not Found".len())
                .body(b"Not Found".to_vec())
                .unwrap(),
        }
    }
}

impl HealthHttpApp {
    fn live_response(&self) -> Response<Vec<u8>> {
        let body = serde_json::json!({
            "status": "healthy"
        });
        let body_str = body.to_string();

        Response::builder()
            .status(200)
            .header(http::header::CONTENT_TYPE, "application/json")
            .header(http::header::CONTENT_LENGTH, body_str.len())
            .body(body_str.into_bytes())
            .unwrap()
    }

    async fn ready_response(&self) -> Response<Vec<u8>> {
        let uptime_ms = self.state.start_time.elapsed().as_millis() as u64;
        let jwks_ready = self.state.jwt_validator.is_ready();
        let valkey_ready =
            !self.state.limiter.is_enabled() || self.state.limiter.ping().await.is_ok();

        // Rate limiting fails open when Valkey is down, so only JWKS gates readiness.
        let (status, http_status) = if jwks_ready {
            ("healthy", 200)
        } else {
            ("unhealthy", 503)
        };

        let body = serde_json::json!({
            "status": status,
            "uptime_ms": uptime_ms,
            "checks": {
                "jwks_ready": jwks_ready,
                "valkey_ready": valkey_ready
            }
        });
        let body_str = body.to_string();

        Response::builder()
            .status(http_status)
            .header(http::header::CONTENT_TYPE, "application/json")
            .header(http::header::CONTENT_LENGTH, body_str.len())
            .body(body_str.into_bytes())
            .unwrap()
    }

    fn metrics_response(&self) -> Response<Vec<u8>> {
        let encoder = TextEncoder::new();
        let metric_families = prometheus::gather();
        let mut buffer = vec![];
        encoder.encode(&metric_families, &mut buffer).unwrap();

        Response::builder()
            .status(200)
            .header(http::header::CONTENT_TYPE, encoder.format_type())
            .header(http::header::CONTENT_LENGTH, buffer.len())
            .body(buffer)
            .unwrap()
    }

    async fn invalidate_authorizer_cache(
        &self,
        http_session: &mut ServerSession,
    ) -> Response<Vec<u8>> {
        let mut body = Vec::new();
        loop {
            match http_session.read_request_body().await {
                Ok(Some(chunk)) => {
                    if body.len() + chunk.len() > MAX_INVALIDATE_BODY {
                        return json_response(
                            413,
                            serde_json::json!({"error": "request body too large"}),
                        );
                    }
                    body.extend_from_slice(&chunk);
                }
                Ok(None) => break,
                Err(_) => {
                    return json_response(
                        400,
                        serde_json::json!({"error": "failed to read request body"}),
                    );
                }
            }
        }
        let req: InvalidateRequest = match serde_json::from_slice(&body) {
            Ok(r) => r,
            Err(_) => {
                return json_response(
                    400,
                    serde_json::json!({"error": "body must be a JSON object"}),
                );
            }
        };
        if req.is_empty() {
            return json_response(
                400,
                serde_json::json!({"error": "set all: true or at least one of authorizer, subject_id, resource_type, resource_id"}),
            );
        }
        self.state.authorizer_invalidator.invalidate(&req);
        json_response(200, serde_json::json!({"invalidated": true}))
    }
}

fn json_response(status: u16, body: serde_json::Value) -> Response<Vec<u8>> {
    let s = body.to_string();
    Response::builder()
        .status(status)
        .header(http::header::CONTENT_TYPE, "application/json")
        .header(http::header::CONTENT_LENGTH, s.len())
        .body(s.into_bytes())
        .unwrap()
}

/// The HttpServer for HealthHttpApp with compression enabled
pub type HealthServer = HttpServer<HealthHttpApp>;

pub fn new_health_server(state: Arc<HealthState>) -> HealthServer {
    let mut server = HealthServer::new_app(HealthHttpApp::new(state));
    server.add_module(ResponseCompressionBuilder::enable(7));
    server
}
