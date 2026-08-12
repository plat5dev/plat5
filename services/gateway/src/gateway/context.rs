use std::time::Instant;

use pingora::upstreams::peer::HttpPeer;
use tracing::Span;

pub struct GatewayContext {
    pub start: Instant,
    pub route: Option<String>,
    pub method: Option<String>,
    pub request_id: Option<String>,
    pub request_origin: Option<String>,
    pub root_span: Option<Span>,
    pub upstream_peer: Option<Box<HttpPeer>>,
    pub body_bytes: u64,
}

impl Default for GatewayContext {
    fn default() -> Self {
        Self::new()
    }
}

impl GatewayContext {
    pub fn new() -> Self {
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

    pub fn route_label(&self) -> &str {
        self.route.as_deref().unwrap_or("unmatched")
    }

    pub fn method_label(&self) -> &str {
        self.method.as_deref().unwrap_or("UNKNOWN")
    }

    pub fn ensure_root_span(&mut self, path: &str, method: &str) -> Span {
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

    pub fn root_span(&self) -> Option<Span> {
        self.root_span.as_ref().cloned()
    }

    pub fn finish_root_span(&mut self) {
        self.root_span = None;
    }
}
