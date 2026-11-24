//! Outbound OpenID AuthZEN 1.0 policy decision point (PDP) client and decision
//! cache (docs/reference/config/authorizers.md).
//!
//! When a route declares an `authorizer`, the gateway delegates (or combines)
//! the authorization check with an external AuthZEN 1.0 PDP via
//! `POST {url}/access/v1/evaluation` (single check) or
//! `POST {url}/access/v1/evaluations` (multi-permission OR routes and batched
//! `POST /access/routes` UI checks).

use std::collections::{BTreeMap, HashMap};
use std::future::Future;
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, RwLock};
use std::time::{Duration, Instant};

use moka::future::Cache;
use moka::ops::compute::{CompResult, Op};
use opentelemetry::global;
use serde::{Deserialize, Serialize};
use serde_json::Value;
use tracing::{warn, Instrument, Span};
use tracing_opentelemetry::OpenTelemetrySpanExt;

use crate::duration::parse_duration;
use crate::metrics;
use crate::route_map::Upstream;

const CACHE_CAPACITY: u64 = 50_000;
/// Maximum items in one AuthZEN `/access/v1/evaluations` call.
pub const MAX_BATCH_EVALUATIONS: usize = 100;

/// Raw authorizer entry from the control plane's gateway feed (`authorizers.<name>`).
#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
pub struct AuthorizerDoc {
    pub url: String,
    pub timeout: String,
    pub cache_ttl: String,
    pub negative_cache_ttl: String,
    #[serde(default)]
    pub token_env: Option<String>,
}

/// Compiled authorizer configuration inside a loaded gateway snapshot.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct AuthorizerSpec {
    pub name: String,
    pub url: String,
    pub timeout: Duration,
    pub cache_ttl: Duration,
    pub negative_cache_ttl: Duration,
    /// Resolved bearer token from `token_env`, if configured.
    pub token: Option<String>,
}

/// How an external authorizer combines with Plat5's built-in RBAC.
#[derive(Clone, Copy, Debug, Default, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub enum AuthorizerMode {
    #[default]
    Replace,
    And,
    Or,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
pub struct RouteResourceDoc {
    #[serde(rename = "type")]
    pub r#type: String,
    pub id: String,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
pub struct RouteAuthorizerDoc {
    pub name: String,
    #[serde(default)]
    pub mode: AuthorizerMode,
    #[serde(default)]
    pub resource: Option<RouteResourceDoc>,
    #[serde(default)]
    pub action: Option<String>,
}

#[derive(Debug)]
pub struct RouteResource {
    pub r#type: String,
    pub id: Upstream,
}

#[derive(Debug)]
pub struct RouteAuthorizer {
    pub name: String,
    pub mode: AuthorizerMode,
    pub resource: Option<RouteResource>,
    pub action: Option<String>,
}

/// Parse and validate all `authorizers` in a control-plane snapshot, resolving
/// any referenced `token_env` from the process environment.
pub fn parse_authorizers(
    docs: &BTreeMap<String, AuthorizerDoc>,
) -> Result<BTreeMap<String, AuthorizerSpec>, String> {
    parse_authorizers_with_env(docs, |k| std::env::var(k).ok())
}

pub fn parse_authorizers_with_env(
    docs: &BTreeMap<String, AuthorizerDoc>,
    lookup_env: impl Fn(&str) -> Option<String>,
) -> Result<BTreeMap<String, AuthorizerSpec>, String> {
    let mut out = BTreeMap::new();
    for (name, doc) in docs {
        let timeout = parse_duration(&doc.timeout)
            .filter(|d| !d.is_zero())
            .ok_or_else(|| {
                format!(
                    "authorizers.{name}.timeout {:?} is invalid (must be >= 1s)",
                    doc.timeout
                )
            })?;
        let cache_ttl = parse_duration(&doc.cache_ttl).ok_or_else(|| {
            format!(
                "authorizers.{name}.cache_ttl {:?} is invalid",
                doc.cache_ttl
            )
        })?;
        let negative_cache_ttl = parse_duration(&doc.negative_cache_ttl).ok_or_else(|| {
            format!(
                "authorizers.{name}.negative_cache_ttl {:?} is invalid",
                doc.negative_cache_ttl
            )
        })?;
        let url = doc.url.trim().trim_end_matches('/').to_string();
        if !(url.starts_with("http://") || url.starts_with("https://")) {
            return Err(format!(
                "authorizers.{name}.url {:?} must be http or https",
                doc.url
            ));
        }
        let token = match &doc.token_env {
            None => None,
            Some(env_name) => {
                let val = lookup_env(env_name)
                    .map(|v| v.trim().to_string())
                    .filter(|v| !v.is_empty())
                    .ok_or_else(|| {
                        format!(
                            "authorizers.{name}.token_env references {env_name}, which is not set"
                        )
                    })?;
                Some(val)
            }
        };
        out.insert(
            name.clone(),
            AuthorizerSpec {
                name: name.clone(),
                url,
                timeout,
                cache_ttl,
                negative_cache_ttl,
                token,
            },
        );
    }
    Ok(out)
}

// --- OpenID AuthZEN 1.0 wire types ---

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
        semantic: &str,
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
        semantic: &str,
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
        _semantic: &str,
        _request_id: &str,
    ) -> Result<Vec<bool>, AuthorizerUnavailable> {
        Err(AuthorizerUnavailable(
            "no authorizer configured".to_string(),
        ))
    }
}

/// Real HTTP AuthZEN 1.0 client.
#[derive(Clone)]
pub struct HttpAuthorizer {
    client: reqwest::Client,
    block_private_ips: bool,
}

impl HttpAuthorizer {
    pub fn new(block_private_ips: bool) -> Self {
        let client = reqwest::Client::builder()
            .build()
            .expect("failed to build authorizer HTTP client");
        Self {
            client,
            block_private_ips,
        }
    }

    async fn check_ssrf(&self, url: &str) -> Result<(), AuthorizerUnavailable> {
        if !self.block_private_ips {
            return Ok(());
        }
        let parsed = reqwest::Url::parse(url)
            .map_err(|e| AuthorizerUnavailable(format!("invalid url: {e}")))?;
        let host = parsed
            .host_str()
            .ok_or_else(|| AuthorizerUnavailable("missing host".to_string()))?;
        let port = parsed.port_or_known_default().unwrap_or(80);
        if let Ok(ip) = host.parse::<IpAddr>() {
            if is_private_or_reserved(ip) {
                return Err(AuthorizerUnavailable("blocked private IP".to_string()));
            }
            return Ok(());
        }
        let addrs = tokio::net::lookup_host((host, port))
            .await
            .map_err(|e| AuthorizerUnavailable(format!("dns lookup failed: {e}")))?;
        let mut saw_any = false;
        for addr in addrs {
            saw_any = true;
            if is_private_or_reserved(addr.ip()) {
                return Err(AuthorizerUnavailable("blocked private IP".to_string()));
            }
        }
        if !saw_any {
            return Err(AuthorizerUnavailable(
                "dns returned no addresses".to_string(),
            ));
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
        self.check_ssrf(&url).await?;
        let start = Instant::now();
        let span = authorizer_span(&spec.name, op, &url);
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
                    &spec.name,
                    op,
                    "error",
                    start.elapsed().as_secs_f64(),
                );
                warn!(authorizer = %spec.name, op, error = %e, "authorizer request failed");
                AuthorizerUnavailable(e.to_string())
            })?;
        let status = response.status();
        span.record("http.response.status_code", status.as_u16());
        if !status.is_success() {
            metrics::record_authorizer_request(
                &spec.name,
                op,
                "error",
                start.elapsed().as_secs_f64(),
            );
            warn!(
                authorizer = %spec.name,
                op,
                status = status.as_u16(),
                "authorizer returned non-2xx"
            );
            return Err(AuthorizerUnavailable(format!("status {}", status.as_u16())));
        }
        response.json::<R>().await.map_err(|e| {
            metrics::record_authorizer_request(
                &spec.name,
                op,
                "error",
                start.elapsed().as_secs_f64(),
            );
            warn!(authorizer = %spec.name, op, error = %e, "authorizer response decode failed");
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
            &spec.name,
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
        semantic: &str,
        request_id: &str,
    ) -> Result<Vec<bool>, AuthorizerUnavailable> {
        if reqs.is_empty() {
            return Ok(Vec::new());
        }
        let start = Instant::now();
        let body = AuthzenBatchRequest {
            options: BatchOptions {
                evaluations_semantic: semantic,
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
        // Under `execute_all`, every item must be answered. Under short-circuit
        // semantics (`permit_on_first_permit` / `deny_on_first_deny`), the PDP
        // may return a prefix ending on the deciding item.
        if semantic == "execute_all" && decisions.len() != reqs.len() {
            metrics::record_authorizer_request(
                &spec.name,
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
                &spec.name,
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
            &spec.name,
            "evaluations",
            "ok",
            start.elapsed().as_secs_f64(),
        );
        Ok(decisions)
    }
}

fn authorizer_span(name: &str, op: &str, url: &str) -> Span {
    tracing::info_span!(
        "authzen_client",
        otel.kind = "client",
        otel.name = %format!("POST /access/v1/{op}"),
        plat5.authorizer = %name,
        http.request.method = "POST",
        url.full = %url,
        http.response.status_code = tracing::field::Empty,
    )
}

fn traced(mut req: reqwest::RequestBuilder, span: &Span) -> reqwest::RequestBuilder {
    let mut headers = HashMap::new();
    let cx = span.context();
    global::get_text_map_propagator(|p| p.inject_context(&cx, &mut headers));
    for (k, v) in headers {
        req = req.header(k, v);
    }
    req
}

fn is_private_or_reserved(ip: IpAddr) -> bool {
    match ip {
        IpAddr::V4(v4) => is_private_v4(v4),
        IpAddr::V6(v6) => {
            if let Some(mapped) = v6.to_ipv4_mapped() {
                return is_private_v4(mapped);
            }
            v6.is_loopback()
                || v6.is_unspecified()
                || (v6.segments()[0] & 0xfe00) == 0xfc00
                || (v6.segments()[0] & 0xffc0) == 0xfe80
                || v6.is_multicast()
                || v6 == Ipv6Addr::LOCALHOST
        }
    }
}

fn is_private_v4(v4: Ipv4Addr) -> bool {
    let o = v4.octets();
    v4.is_loopback()
        || v4.is_private()
        || v4.is_link_local()
        || v4.is_unspecified()
        || v4.is_broadcast()
        || v4.is_multicast()
        || (o[0] == 100 && (o[1] & 0xc0) == 64)
}

// --- Decision cache + active invalidation ---

/// Request payload for `POST /internal/authorizer-cache/invalidate`.
#[derive(Clone, Debug, Default, Deserialize)]
pub struct InvalidateRequest {
    #[serde(default)]
    pub authorizer: Option<String>,
    #[serde(default)]
    pub subject_id: Option<String>,
    #[serde(default)]
    pub resource_type: Option<String>,
    #[serde(default)]
    pub resource_id: Option<String>,
    #[serde(default)]
    pub all: bool,
}

impl InvalidateRequest {
    pub fn is_empty(&self) -> bool {
        !self.all
            && self.authorizer.is_none()
            && self.subject_id.is_none()
            && self.resource_type.is_none()
            && self.resource_id.is_none()
    }
}

#[derive(Default)]
struct EpochMaps {
    by_authorizer: HashMap<String, u64>,
    by_subject: HashMap<String, u64>,
    by_resource_type: HashMap<String, u64>,
    by_resource: HashMap<(String, String), u64>,
}

/// Shared lock-free/low-contention epoch tracker for active cache invalidations.
#[derive(Clone, Default)]
pub struct AuthorizerInvalidator {
    next_epoch: Arc<AtomicU64>,
    all_epoch: Arc<AtomicU64>,
    maps: Arc<RwLock<EpochMaps>>,
}

impl AuthorizerInvalidator {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn current_epoch(&self) -> u64 {
        self.next_epoch.load(Ordering::Acquire)
    }

    pub fn invalidate(&self, req: &InvalidateRequest) {
        let epoch = self.next_epoch.fetch_add(1, Ordering::AcqRel) + 1;
        if req.all {
            self.all_epoch.store(epoch, Ordering::Release);
            if let Ok(mut maps) = self.maps.write() {
                maps.by_authorizer.clear();
                maps.by_subject.clear();
                maps.by_resource_type.clear();
                maps.by_resource.clear();
            }
            return;
        }
        if let Ok(mut maps) = self.maps.write() {
            if let Some(a) = req.authorizer.as_deref().filter(|s| !s.is_empty()) {
                maps.by_authorizer.insert(a.to_string(), epoch);
            }
            if let Some(s) = req.subject_id.as_deref().filter(|s| !s.is_empty()) {
                maps.by_subject.insert(s.to_string(), epoch);
            }
            match (
                req.resource_type.as_deref().filter(|s| !s.is_empty()),
                req.resource_id.as_deref().filter(|s| !s.is_empty()),
            ) {
                (Some(rt), Some(rid)) => {
                    maps.by_resource
                        .insert((rt.to_string(), rid.to_string()), epoch);
                }
                (Some(rt), None) => {
                    maps.by_resource_type.insert(rt.to_string(), epoch);
                }
                _ => {}
            }
        }
    }

    fn is_fresh(
        &self,
        entry_epoch: u64,
        authorizer: &str,
        subject_id: &str,
        resource_type: &str,
        resource_id: &str,
    ) -> bool {
        if entry_epoch < self.all_epoch.load(Ordering::Acquire) {
            return false;
        }
        let Ok(maps) = self.maps.read() else {
            return false;
        };
        if maps
            .by_authorizer
            .get(authorizer)
            .is_some_and(|e| entry_epoch < *e)
        {
            return false;
        }
        if maps
            .by_subject
            .get(subject_id)
            .is_some_and(|e| entry_epoch < *e)
        {
            return false;
        }
        if maps
            .by_resource_type
            .get(resource_type)
            .is_some_and(|e| entry_epoch < *e)
        {
            return false;
        }
        if maps
            .by_resource
            .get(&(resource_type.to_string(), resource_id.to_string()))
            .is_some_and(|e| entry_epoch < *e)
        {
            return false;
        }
        true
    }
}

#[derive(Clone)]
struct CachedDecision {
    decision: bool,
    ttl: Duration,
    epoch: u64,
}

struct DecisionExpiry;

impl moka::Expiry<String, CachedDecision> for DecisionExpiry {
    fn expire_after_create(
        &self,
        _key: &String,
        value: &CachedDecision,
        _created_at: Instant,
    ) -> Option<Duration> {
        Some(value.ttl)
    }

    fn expire_after_update(
        &self,
        _key: &String,
        value: &CachedDecision,
        _updated_at: Instant,
        _duration_until_expiry: Option<Duration>,
    ) -> Option<Duration> {
        Some(value.ttl)
    }
}

/// Optional per-decision Moka cache wrapping an [`Authorizer`].
/// When `spec.cache_ttl` (for allow) or `spec.negative_cache_ttl` (for deny) is
/// zero, that decision is never stored in the cache.
pub struct AuthorizerCache<P> {
    pdp: P,
    entries: Cache<String, CachedDecision>,
    invalidator: AuthorizerInvalidator,
}

impl<P: Authorizer> AuthorizerCache<P> {
    pub fn new(pdp: P, invalidator: AuthorizerInvalidator) -> Self {
        let entries = Cache::builder()
            .max_capacity(CACHE_CAPACITY)
            .expire_after(DecisionExpiry)
            .build();
        Self {
            pdp,
            entries,
            invalidator,
        }
    }

    pub fn invalidator(&self) -> AuthorizerInvalidator {
        self.invalidator.clone()
    }

    fn cache_key(spec: &AuthorizerSpec, req: &AuthzenRequest) -> String {
        let mut h = blake3::Hasher::new();
        hash_part(&mut h, &spec.name);
        hash_part(&mut h, &spec.url);
        hash_part(&mut h, &req.subject.r#type);
        hash_part(&mut h, &req.subject.id);
        hash_part(&mut h, &req.action.name);
        hash_part(&mut h, &req.resource.r#type);
        hash_part(&mut h, &req.resource.id);
        if let Some(root) = req
            .resource
            .properties
            .get("root_id")
            .and_then(Value::as_str)
        {
            hash_part(&mut h, root);
        }
        h.finalize().to_hex().to_string()
    }

    fn entry_is_fresh(
        &self,
        entry: &CachedDecision,
        spec: &AuthorizerSpec,
        req: &AuthzenRequest,
    ) -> bool {
        self.invalidator.is_fresh(
            entry.epoch,
            &spec.name,
            &req.subject.id,
            &req.resource.r#type,
            &req.resource.id,
        )
    }

    fn ttl_for(spec: &AuthorizerSpec, decision: bool) -> Duration {
        if decision {
            spec.cache_ttl
        } else {
            spec.negative_cache_ttl
        }
    }

    pub async fn evaluate(
        &self,
        spec: &AuthorizerSpec,
        req: &AuthzenRequest,
        request_id: &str,
    ) -> Result<bool, AuthorizerUnavailable> {
        let caching = !spec.cache_ttl.is_zero() || !spec.negative_cache_ttl.is_zero();
        if !caching {
            return self.pdp.evaluate(spec, req, request_id).await;
        }
        let key = Self::cache_key(spec, req);
        if let Some(entry) = self.entries.get(&key).await {
            if self.entry_is_fresh(&entry, spec, req) {
                metrics::record_authorizer_cache(&spec.name, "hit");
                return Ok(entry.decision);
            }
        }
        metrics::record_authorizer_cache(&spec.name, "miss");

        enum EvalErr {
            Unavailable(AuthorizerUnavailable),
            Uncached(bool),
        }

        let computed = self
            .entries
            .entry(key)
            .and_try_compute_with(|maybe| async move {
                if let Some(existing) = maybe {
                    if self.entry_is_fresh(existing.value(), spec, req) {
                        return Ok(Op::Nop);
                    }
                }
                let epoch = self.invalidator.current_epoch();
                let decision = self
                    .pdp
                    .evaluate(spec, req, request_id)
                    .await
                    .map_err(EvalErr::Unavailable)?;
                let ttl = Self::ttl_for(spec, decision);
                if ttl.is_zero() {
                    return Err(EvalErr::Uncached(decision));
                }
                Ok(Op::Put(CachedDecision {
                    decision,
                    ttl,
                    epoch,
                }))
            })
            .await;

        match computed {
            Ok(CompResult::Inserted(e))
            | Ok(CompResult::ReplacedWith(e))
            | Ok(CompResult::Unchanged(e)) => Ok(e.into_value().decision),
            Ok(CompResult::StillNone(_)) | Ok(CompResult::Removed(_)) => {
                Err(AuthorizerUnavailable("cache entry removed".to_string()))
            }
            Err(EvalErr::Uncached(d)) => Ok(d),
            Err(EvalErr::Unavailable(e)) => Err(e),
        }
    }

    /// Evaluates a batch of requests for one authorizer, serving fresh cached
    /// entries locally and sending only misses in chunked `/access/v1/evaluations`
    /// calls.
    pub async fn evaluate_batch(
        &self,
        spec: &AuthorizerSpec,
        reqs: &[AuthzenRequest],
        semantic: &str,
        request_id: &str,
    ) -> Result<Vec<bool>, AuthorizerUnavailable> {
        if reqs.is_empty() {
            return Ok(Vec::new());
        }
        let caching = !spec.cache_ttl.is_zero() || !spec.negative_cache_ttl.is_zero();
        let mut results: Vec<Option<bool>> = vec![None; reqs.len()];
        let mut misses: Vec<usize> = Vec::new();
        if caching {
            for (i, req) in reqs.iter().enumerate() {
                let key = Self::cache_key(spec, req);
                if let Some(entry) = self.entries.get(&key).await {
                    if self.entry_is_fresh(&entry, spec, req) {
                        metrics::record_authorizer_cache(&spec.name, "hit");
                        results[i] = Some(entry.decision);
                        if semantic == "permit_on_first_permit" && entry.decision {
                            return Ok(vec![true]);
                        }
                        if semantic == "deny_on_first_deny" && !entry.decision {
                            return Ok(vec![false]);
                        }
                        continue;
                    }
                }
                metrics::record_authorizer_cache(&spec.name, "miss");
                misses.push(i);
            }
        } else {
            misses.extend(0..reqs.len());
        }

        if !misses.is_empty() {
            for chunk in misses.chunks(MAX_BATCH_EVALUATIONS) {
                let epoch = self.invalidator.current_epoch();
                let batch_reqs: Vec<AuthzenRequest> =
                    chunk.iter().map(|&idx| reqs[idx].clone()).collect();
                let decisions = self
                    .pdp
                    .evaluate_batch(spec, &batch_reqs, semantic, request_id)
                    .await?;
                for (&idx, &decision) in chunk.iter().zip(decisions.iter()) {
                    results[idx] = Some(decision);
                    if caching {
                        let ttl = Self::ttl_for(spec, decision);
                        if !ttl.is_zero() {
                            let key = Self::cache_key(spec, &reqs[idx]);
                            self.entries
                                .insert(
                                    key,
                                    CachedDecision {
                                        decision,
                                        ttl,
                                        epoch,
                                    },
                                )
                                .await;
                        }
                    }
                    if semantic == "permit_on_first_permit" && decision {
                        return Ok(vec![true]);
                    }
                    if semantic == "deny_on_first_deny" && !decision {
                        return Ok(vec![false]);
                    }
                }
            }
        }

        Ok(results.into_iter().map(|r| r.unwrap_or(false)).collect())
    }
}

fn hash_part(h: &mut blake3::Hasher, s: &str) {
    h.update(&(s.len() as u64).to_le_bytes());
    h.update(s.as_bytes());
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::AtomicUsize;

    struct StubPdp {
        calls: Arc<AtomicUsize>,
        decision: Arc<std::sync::Mutex<bool>>,
    }

    impl Authorizer for StubPdp {
        async fn evaluate(
            &self,
            _spec: &AuthorizerSpec,
            _req: &AuthzenRequest,
            _request_id: &str,
        ) -> Result<bool, AuthorizerUnavailable> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            Ok(*self.decision.lock().unwrap())
        }

        async fn evaluate_batch(
            &self,
            _spec: &AuthorizerSpec,
            reqs: &[AuthzenRequest],
            _semantic: &str,
            _request_id: &str,
        ) -> Result<Vec<bool>, AuthorizerUnavailable> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            let d = *self.decision.lock().unwrap();
            Ok(vec![d; reqs.len()])
        }
    }

    fn sample_spec(cache_secs: u64) -> AuthorizerSpec {
        AuthorizerSpec {
            name: "topaz".into(),
            url: "http://topaz:8383".into(),
            timeout: Duration::from_secs(2),
            cache_ttl: Duration::from_secs(cache_secs),
            negative_cache_ttl: Duration::from_secs(cache_secs),
            token: None,
        }
    }

    fn sample_req(subject_id: &str, doc_id: &str) -> AuthzenRequest {
        AuthzenRequest {
            subject: AuthzenSubject {
                r#type: "member".into(),
                id: subject_id.into(),
                properties: BTreeMap::new(),
            },
            action: AuthzenAction {
                name: "docs:read".into(),
                properties: BTreeMap::new(),
            },
            resource: AuthzenResource {
                r#type: "document".into(),
                id: doc_id.into(),
                properties: BTreeMap::new(),
            },
            context: BTreeMap::new(),
        }
    }

    #[test]
    fn parse_authorizers_resolves_token_env_and_refuses_missing() {
        let mut docs = BTreeMap::new();
        docs.insert(
            "topaz".into(),
            AuthorizerDoc {
                url: "http://topaz:8383/".into(),
                timeout: "2s".into(),
                cache_ttl: "0s".into(),
                negative_cache_ttl: "5s".into(),
                token_env: Some("TOPAZ_TOKEN".into()),
            },
        );
        let ok = parse_authorizers_with_env(&docs, |k| {
            (k == "TOPAZ_TOKEN").then(|| "secret-123".to_string())
        })
        .unwrap();
        assert_eq!(ok["topaz"].url, "http://topaz:8383");
        assert_eq!(ok["topaz"].token.as_deref(), Some("secret-123"));
        assert_eq!(ok["topaz"].cache_ttl, Duration::ZERO);
        assert_eq!(ok["topaz"].negative_cache_ttl, Duration::from_secs(5));

        let err = parse_authorizers_with_env(&docs, |_| None).unwrap_err();
        assert!(err.contains("TOPAZ_TOKEN"), "{err}");
    }

    #[tokio::test]
    async fn cache_hits_and_targeted_invalidation() {
        let calls = Arc::new(AtomicUsize::new(0));
        let decision = Arc::new(std::sync::Mutex::new(true));
        let invalidator = AuthorizerInvalidator::new();
        let cache = AuthorizerCache::new(
            StubPdp {
                calls: calls.clone(),
                decision: decision.clone(),
            },
            invalidator.clone(),
        );
        let spec = sample_spec(60);
        let req = sample_req("01m1", "doc-1");

        assert!(cache.evaluate(&spec, &req, "r1").await.unwrap());
        assert!(cache.evaluate(&spec, &req, "r2").await.unwrap());
        assert_eq!(calls.load(Ordering::SeqCst), 1);

        // Invalidate another resource: still cached.
        invalidator.invalidate(&InvalidateRequest {
            resource_type: Some("document".into()),
            resource_id: Some("doc-2".into()),
            ..Default::default()
        });
        assert!(cache.evaluate(&spec, &req, "r3").await.unwrap());
        assert_eq!(calls.load(Ordering::SeqCst), 1);

        // Flip PDP to deny and invalidate doc-1: cache misses and returns false.
        *decision.lock().unwrap() = false;
        invalidator.invalidate(&InvalidateRequest {
            resource_type: Some("document".into()),
            resource_id: Some("doc-1".into()),
            ..Default::default()
        });
        assert!(!cache.evaluate(&spec, &req, "r4").await.unwrap());
        assert_eq!(calls.load(Ordering::SeqCst), 2);
    }

    #[tokio::test]
    async fn zero_ttl_never_caches() {
        let calls = Arc::new(AtomicUsize::new(0));
        let cache = AuthorizerCache::new(
            StubPdp {
                calls: calls.clone(),
                decision: Arc::new(std::sync::Mutex::new(true)),
            },
            AuthorizerInvalidator::new(),
        );
        let spec = sample_spec(0);
        let req = sample_req("01m1", "doc-1");
        assert!(cache.evaluate(&spec, &req, "r1").await.unwrap());
        assert!(cache.evaluate(&spec, &req, "r2").await.unwrap());
        assert_eq!(calls.load(Ordering::SeqCst), 2);
    }
}
