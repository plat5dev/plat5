use async_trait::async_trait;
use http::Response;
use pingora::apps::http_app::{HttpServer, ServeHttp};
use pingora::modules::http::compression::ResponseCompressionBuilder;
use pingora::protocols::http::ServerSession;
use prometheus::{Encoder, TextEncoder};
use std::sync::Arc;
use std::time::Instant;

use crate::auth::jwt::JwtValidatorState;
use crate::rate_limit::RateLimiter;

/// Shared state for health checks
pub struct HealthState {
    start_time: Instant,
    jwt_validator: JwtValidatorState,
    limiter: RateLimiter,
}

impl HealthState {
    pub fn new(jwt_validator: JwtValidatorState, limiter: RateLimiter) -> Self {
        Self {
            start_time: Instant::now(),
            jwt_validator,
            limiter,
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
        let path = http_session.req_header().uri.path();

        match path {
            "/health/live" => self.live_response(),
            "/health/ready" => self.ready_response().await,
            "/metrics" => self.metrics_response(),
            _ => Response::builder()
                .status(404)
                .header(http::header::CONTENT_TYPE, "text/plain")
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
        let redis_ready = self.state.limiter.ping().await.is_ok();

        let (status, http_status) = if jwks_ready && redis_ready {
            ("ready", 200)
        } else {
            ("not_ready", 503)
        };

        let body = serde_json::json!({
            "status": status,
            "uptime_ms": uptime_ms,
            "checks": {
                "jwks_ready": jwks_ready,
                "redis_ready": redis_ready
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
}

/// The HttpServer for HealthHttpApp with compression enabled
pub type HealthServer = HttpServer<HealthHttpApp>;

pub fn new_health_server(state: Arc<HealthState>) -> HealthServer {
    let mut server = HealthServer::new_app(HealthHttpApp::new(state));
    server.add_module(ResponseCompressionBuilder::enable(7));
    server
}
