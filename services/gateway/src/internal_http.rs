use std::collections::HashMap;
use std::time::Duration;

use opentelemetry::global;
use serde::de::DeserializeOwned;
use serde::Serialize;
use tracing::{Instrument, Span};
use tracing_opentelemetry::OpenTelemetrySpanExt;

const INTERNAL_TOKEN_HEADER: &str = "X-Plat5-Internal-Token";
const DEFAULT_TIMEOUT_SECS: u64 = 5;

/// Shared HTTP client for gateway → identity (and similar) internal calls.
/// Domain modules keep their own request/response types; this only does transport.
#[derive(Clone)]
pub struct InternalHttpClient {
    client: reqwest::Client,
    internal_token: Option<String>,
}

#[derive(Debug)]
pub enum InternalHttpError {
    Network(String),
    /// Non-success HTTP status (body not decoded as domain type).
    HttpStatus {
        status: u16,
    },
    Decode(String),
}

impl InternalHttpError {
    pub fn is_not_found(&self) -> bool {
        matches!(self, InternalHttpError::HttpStatus { status: 404 })
    }
}

impl InternalHttpClient {
    pub fn new(internal_token: Option<String>) -> Self {
        Self::with_timeout(Duration::from_secs(DEFAULT_TIMEOUT_SECS), internal_token)
    }

    pub fn with_timeout(timeout: Duration, internal_token: Option<String>) -> Self {
        let client = reqwest::Client::builder()
            .timeout(timeout)
            .build()
            .expect("failed to create reqwest client");

        Self {
            client,
            internal_token,
        }
    }

    /// Send JSON with `method` and a per-call `timeout`. Returns the status, 2xx or not;
    /// only transport failures are errors. The body is not read.
    pub async fn send_json<B>(
        &self,
        method: reqwest::Method,
        url: &str,
        body: &B,
        timeout: Duration,
    ) -> Result<u16, InternalHttpError>
    where
        B: Serialize + ?Sized,
    {
        let span = client_span(&method, url);
        let mut req = self.client.request(method, url).timeout(timeout).json(body);
        if let Some(token) = &self.internal_token {
            req = req.header(INTERNAL_TOKEN_HEADER, token);
        }
        let result = traced(req, &span)
            .send()
            .instrument(span.clone())
            .await
            .map(|response| response.status().as_u16())
            .map_err(|e| InternalHttpError::Network(e.to_string()));
        if let Ok(status) = result {
            span.record("http.response.status_code", status);
        }
        result
    }

    /// POST JSON. On 2xx, deserialize body as `R`. Otherwise `HttpStatus` (no body parse).
    pub async fn post_json<B, R>(&self, url: &str, body: &B) -> Result<R, InternalHttpError>
    where
        B: Serialize + ?Sized,
        R: DeserializeOwned,
    {
        let span = client_span(&reqwest::Method::POST, url);
        let mut req = self.client.post(url).json(body);
        if let Some(token) = &self.internal_token {
            req = req.header(INTERNAL_TOKEN_HEADER, token);
        }

        let response = traced(req, &span)
            .send()
            .instrument(span.clone())
            .await
            .map_err(|e| InternalHttpError::Network(e.to_string()))?;

        let status = response.status();
        span.record("http.response.status_code", status.as_u16());
        if !status.is_success() {
            return Err(InternalHttpError::HttpStatus {
                status: status.as_u16(),
            });
        }

        response
            .json::<R>()
            .await
            .map_err(|e| InternalHttpError::Decode(e.to_string()))
    }
}

/// A client span for one internal call, a child of the current span: the
/// request's, so identity's and audit's spans land in the caller's trace.
fn client_span(method: &reqwest::Method, url: &str) -> Span {
    let path = reqwest::Url::parse(url)
        .map(|u| u.path().to_string())
        .unwrap_or_default();
    tracing::info_span!(
        "internal_http",
        otel.kind = "client",
        otel.name = %format!("{method} {path}"),
        http.request.method = %method,
        url.full = %url,
        http.response.status_code = tracing::field::Empty,
    )
}

/// `req` with `span`'s trace context (`traceparent`).
fn traced(mut req: reqwest::RequestBuilder, span: &Span) -> reqwest::RequestBuilder {
    let mut headers = HashMap::new();
    let cx = span.context();
    global::get_text_map_propagator(|p| p.inject_context(&cx, &mut headers));
    for (k, v) in headers {
        req = req.header(k, v);
    }
    req
}
