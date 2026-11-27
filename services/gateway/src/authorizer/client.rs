//! OpenID AuthZEN 1.0 HTTP client and trait.

use std::collections::BTreeMap;
use std::future::Future;
use std::sync::Arc;
use std::time::Instant;

use serde::{Deserialize, Serialize};
use serde_json::Value;
use tracing::{warn, Instrument, Span};

use crate::internal_http::traced;
use crate::metrics;
use crate::net::{is_blocked_ip, PeerResolver};

use super::config::AuthorizerSpec;

/// Maximum items in one AuthZEN `/access/v1/evaluations` call.
pub const MAX_BATCH_EVALUATIONS: usize = 100;

/// Metric and span label for the single deployment-wide external authorizer.
pub const AUTHORIZER_METRIC_LABEL: &str = "external";

/// AuthZEN batch `evaluations_semantic` values used by the gateway.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum BatchSemantic {
    ExecuteAll,
    PermitOnFirstPermit,
}

impl BatchSemantic {
    pub fn as_str(self) -> &'static str {
        match self {
            BatchSemantic::ExecuteAll => "execute_all",
            BatchSemantic::PermitOnFirstPermit => "permit_on_first_permit",
        }
    }
}

#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct AuthzenSubject {
    #[serde(rename = "type")]
    pub r#type: String,
    pub id: String,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    pub properties: BTreeMap<String, Value>,
}

#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct AuthzenAction {
    pub name: String,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    pub properties: BTreeMap<String, Value>,
}

#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct AuthzenResource {
    #[serde(rename = "type")]
    pub r#type: String,
    pub id: String,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    pub properties: BTreeMap<String, Value>,
}

#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct AuthzenRequest {
    pub subject: AuthzenSubject,
    pub action: AuthzenAction,
    pub resource: AuthzenResource,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    pub context: BTreeMap<String, Value>,
}

#[derive(Serialize)]
struct BatchOptions<'a> {
    evaluations_semantic: &'a str,
}

#[derive(Serialize)]
struct AuthzenBatchRequest<'a> {
    options: BatchOptions<'a>,
    evaluations: &'a [AuthzenRequest],
}

#[derive(Deserialize)]
struct AuthzenResponse {
    decision: bool,
}

#[derive(Deserialize)]
struct AuthzenBatchResponse {
    evaluations: Vec<AuthzenResponse>,
}

/// External PDP didn't answer usably: timeout, transport, non-2xx, or malformed body (**503**).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct AuthorizerUnavailable(pub String);

pub trait Authorizer: Send + Sync {
    fn evaluate(
        &self,
        spec: &AuthorizerSpec,
        req: &AuthzenRequest,
        request_id: &str,
    ) -> impl Future<Output = Result<bool, AuthorizerUnavailable>> + Send;

    fn evaluate_batch(
        &self,
        spec: &AuthorizerSpec,
        reqs: &[AuthzenRequest],
        semantic: BatchSemantic,
        request_id: &str,
    ) -> impl Future<Output = Result<Vec<bool>, AuthorizerUnavailable>> + Send;
}

impl<P: Authorizer> Authorizer for Arc<P> {
    async fn evaluate(
        &self,
        spec: &AuthorizerSpec,
        req: &AuthzenRequest,
        request_id: &str,
    ) -> Result<bool, AuthorizerUnavailable> {
        (**self).evaluate(spec, req, request_id).await
    }

    async fn evaluate_batch(
        &self,
        spec: &AuthorizerSpec,
        reqs: &[AuthzenRequest],
        semantic: BatchSemantic,
        request_id: &str,
    ) -> Result<Vec<bool>, AuthorizerUnavailable> {
        (**self)
            .evaluate_batch(spec, reqs, semantic, request_id)
            .await
    }
}

/// Placeholder authorizer used when no external PDP client is wired (e.g. unit tests
/// that only test built-in RBAC).
#[derive(Clone, Copy, Default)]
pub struct NoAuthorizer;

impl Authorizer for NoAuthorizer {
    async fn evaluate(
        &self,
        _spec: &AuthorizerSpec,
        _req: &AuthzenRequest,
        _request_id: &str,
    ) -> Result<bool, AuthorizerUnavailable> {
        Err(AuthorizerUnavailable(
            "no authorizer configured".to_string(),
        ))
    }

    async fn evaluate_batch(
        &self,
        _spec: &AuthorizerSpec,
        _reqs: &[AuthzenRequest],
        _semantic: BatchSemantic,
        _request_id: &str,
    ) -> Result<Vec<bool>, AuthorizerUnavailable> {
        Err(AuthorizerUnavailable(
            "no authorizer configured".to_string(),
        ))
    }
}

/// Real HTTP AuthZEN 1.0 client backed by the shared cached SSRF-safe [`PeerResolver`].
#[derive(Clone)]
pub struct HttpAuthorizer {
    client: reqwest::Client,
    resolver: PeerResolver,
}

impl HttpAuthorizer {
    pub fn new(resolver: PeerResolver) -> Self {
        let client = reqwest::Client::builder()
            .dns_resolver(Arc::new(resolver.clone()))
            .redirect(reqwest::redirect::Policy::none())
            .build()
            .expect("failed to build authorizer HTTP client");
        Self { client, resolver }
    }

    /// Reject literal private/reserved IP URLs up front (reqwest bypasses its
    /// `dns_resolver` for IP literals; hostnames are checked and pinned inside
    /// `PeerResolver` without TOCTOU re-resolution).
    fn check_ip_literal(&self, url: &str) -> Result<(), AuthorizerUnavailable> {
        if !self.resolver.block_private_ips() {
            return Ok(());
        }
        let parsed = reqwest::Url::parse(url)
            .map_err(|e| AuthorizerUnavailable(format!("invalid url: {e}")))?;
        if let Some(host) = parsed.host_str() {
            let trimmed = host.trim_start_matches('[').trim_end_matches(']');
            if let Ok(ip) = trimmed.parse::<std::net::IpAddr>() {
                if !self.resolver.is_allowed_private_host(trimmed) && is_blocked_ip(ip) {
                    return Err(AuthorizerUnavailable(format!(
                        "blocked private or reserved IP {ip}"
                    )));
                }
            }
        }
        Ok(())
    }

    async fn post_json<B: Serialize + ?Sized, R: for<'de> Deserialize<'de>>(
        &self,
        spec: &AuthorizerSpec,
        op: &str,
        path: &str,
        body: &B,
        request_id: &str,
    ) -> Result<R, AuthorizerUnavailable> {
        let url = format!("{}{}", spec.url, path);
        let start = Instant::now();
        if let Err(err) = self.check_ip_literal(&url) {
            metrics::record_authorizer_request(
                AUTHORIZER_METRIC_LABEL,
                op,
                "error",
                start.elapsed().as_secs_f64(),
            );
            warn!(op, error = %err.0, "authorizer request blocked");
            return Err(err);
        }
        let span = authorizer_span(op, &url);
        let mut req = self
            .client
            .post(&url)
            .timeout(spec.timeout)
            .header("X-Request-ID", request_id)
            .json(body);
        if let Some(token) = &spec.token {
            req = req.bearer_auth(token);
        }
        let response = traced(req, &span)
            .send()
            .instrument(span.clone())
            .await
            .map_err(|e| {
                metrics::record_authorizer_request(
                    AUTHORIZER_METRIC_LABEL,
                    op,
                    "error",
                    start.elapsed().as_secs_f64(),
                );
                warn!(op, error = %e, "authorizer request failed");
                AuthorizerUnavailable(e.to_string())
            })?;
        let status = response.status();
        span.record("http.response.status_code", status.as_u16());
        if !status.is_success() {
            metrics::record_authorizer_request(
                AUTHORIZER_METRIC_LABEL,
                op,
                "error",
                start.elapsed().as_secs_f64(),
            );
            warn!(op, status = status.as_u16(), "authorizer returned non-2xx");
            return Err(AuthorizerUnavailable(format!("status {}", status.as_u16())));
        }
        response.json::<R>().await.map_err(|e| {
            metrics::record_authorizer_request(
                AUTHORIZER_METRIC_LABEL,
                op,
                "error",
                start.elapsed().as_secs_f64(),
            );
            warn!(op, error = %e, "authorizer response decode failed");
            AuthorizerUnavailable(e.to_string())
        })
    }
}

impl Authorizer for HttpAuthorizer {
    async fn evaluate(
        &self,
        spec: &AuthorizerSpec,
        req: &AuthzenRequest,
        request_id: &str,
    ) -> Result<bool, AuthorizerUnavailable> {
        let start = Instant::now();
        let resp: AuthzenResponse = self
            .post_json(spec, "evaluation", "/access/v1/evaluation", req, request_id)
            .await?;
        let outcome = if resp.decision { "allow" } else { "deny" };
        metrics::record_authorizer_request(
            AUTHORIZER_METRIC_LABEL,
            "evaluation",
            outcome,
            start.elapsed().as_secs_f64(),
        );
        Ok(resp.decision)
    }

    async fn evaluate_batch(
        &self,
        spec: &AuthorizerSpec,
        reqs: &[AuthzenRequest],
        semantic: BatchSemantic,
        request_id: &str,
    ) -> Result<Vec<bool>, AuthorizerUnavailable> {
        if reqs.is_empty() {
            return Ok(Vec::new());
        }
        let start = Instant::now();
        let body = AuthzenBatchRequest {
            options: BatchOptions {
                evaluations_semantic: semantic.as_str(),
            },
            evaluations: reqs,
        };
        let resp: AuthzenBatchResponse = self
            .post_json(
                spec,
                "evaluations",
                "/access/v1/evaluations",
                &body,
                request_id,
            )
            .await?;
        let decisions: Vec<bool> = resp.evaluations.into_iter().map(|e| e.decision).collect();
        if semantic == BatchSemantic::ExecuteAll && decisions.len() != reqs.len() {
            metrics::record_authorizer_request(
                AUTHORIZER_METRIC_LABEL,
                "evaluations",
                "error",
                start.elapsed().as_secs_f64(),
            );
            return Err(AuthorizerUnavailable(format!(
                "expected {} evaluations, got {}",
                reqs.len(),
                decisions.len()
            )));
        }
        if decisions.is_empty() || decisions.len() > reqs.len() {
            metrics::record_authorizer_request(
                AUTHORIZER_METRIC_LABEL,
                "evaluations",
                "error",
                start.elapsed().as_secs_f64(),
            );
            return Err(AuthorizerUnavailable(format!(
                "unexpected evaluations count {}",
                decisions.len()
            )));
        }
        metrics::record_authorizer_request(
            AUTHORIZER_METRIC_LABEL,
            "evaluations",
            "ok",
            start.elapsed().as_secs_f64(),
        );
        Ok(decisions)
    }
}

fn authorizer_span(op: &str, url: &str) -> Span {
    tracing::info_span!(
        "authzen_client",
        otel.kind = "client",
        otel.name = %format!("POST /access/v1/{op}"),
        plat5.authorizer = AUTHORIZER_METRIC_LABEL,
        http.request.method = "POST",
        url.full = %url,
        http.response.status_code = tracing::field::Empty,
    )
}
