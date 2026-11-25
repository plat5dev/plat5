//! TTL-bounded AuthZEN decision cache.
//!
//! Caching is opt-in per authorizer via `cache_ttl` and `negative_cache_ttl`
//! (both default to `0s`). Keys hash the full decision-relevant request
//! (subject, action, resource, all properties, and non-unique context fields)
//! so sibling child nodes, HTTP methods, and context params never collide.

use std::collections::BTreeMap;
use std::time::{Duration, Instant};

use moka::future::Cache;
use moka::ops::compute::{CompResult, Op};
use serde_json::Value;

use crate::auth::admit::hash_part;
use crate::metrics;

use super::client::{
    Authorizer, AuthorizerUnavailable, AuthzenRequest, BatchSemantic, MAX_BATCH_EVALUATIONS,
};
use super::config::AuthorizerSpec;

const CACHE_CAPACITY: u64 = 50_000;

#[derive(Clone)]
struct CachedDecision {
    decision: bool,
    ttl: Duration,
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

/// Optional per-decision Moka TTL cache wrapping an [`Authorizer`].
/// When `spec.cache_ttl` (for allow) or `spec.negative_cache_ttl` (for deny) is
/// zero, that decision is never stored in the cache.
pub struct AuthorizerCache<P> {
    pdp: P,
    entries: Cache<String, CachedDecision>,
}

impl<P: Authorizer> AuthorizerCache<P> {
    pub fn new(pdp: P) -> Self {
        let entries = Cache::builder()
            .max_capacity(CACHE_CAPACITY)
            .expire_after(DecisionExpiry)
            .build();
        Self { pdp, entries }
    }

    fn cache_key(spec: &AuthorizerSpec, req: &AuthzenRequest) -> String {
        let mut h = blake3::Hasher::new();
        hash_part(&mut h, &spec.name);
        hash_part(&mut h, &spec.url);
        hash_part(&mut h, &req.subject.r#type);
        hash_part(&mut h, &req.subject.id);
        hash_json_map(&mut h, "sp", &req.subject.properties, None);
        hash_part(&mut h, &req.action.name);
        hash_json_map(&mut h, "ap", &req.action.properties, None);
        hash_part(&mut h, &req.resource.r#type);
        hash_part(&mut h, &req.resource.id);
        hash_json_map(&mut h, "rp", &req.resource.properties, None);
        // Skip per-request `request_id` so identical requests share cache entries.
        hash_json_map(&mut h, "ctx", &req.context, Some("request_id"));
        h.finalize().to_hex().to_string()
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
            metrics::record_authorizer_cache(&spec.name, "hit");
            return Ok(entry.decision);
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
                if maybe.is_some() {
                    return Ok(Op::Nop);
                }
                let decision = self
                    .pdp
                    .evaluate(spec, req, request_id)
                    .await
                    .map_err(EvalErr::Unavailable)?;
                let ttl = Self::ttl_for(spec, decision);
                if ttl.is_zero() {
                    return Err(EvalErr::Uncached(decision));
                }
                Ok(Op::Put(CachedDecision { decision, ttl }))
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

    /// Evaluates a batch of requests for one authorizer, returning one decision
    /// per input request (`ExecuteAll`) via chunked `/access/v1/evaluations` calls.
    pub async fn evaluate_all(
        &self,
        spec: &AuthorizerSpec,
        reqs: &[AuthzenRequest],
        request_id: &str,
    ) -> Result<Vec<bool>, AuthorizerUnavailable> {
        self.evaluate_batch_inner(spec, reqs, BatchSemantic::ExecuteAll, request_id)
            .await
    }

    /// Evaluates a set of candidate actions for one route, returning `true` as
    /// soon as any candidate permits (`PermitOnFirstPermit`).
    pub async fn evaluate_any(
        &self,
        spec: &AuthorizerSpec,
        reqs: &[AuthzenRequest],
        request_id: &str,
    ) -> Result<bool, AuthorizerUnavailable> {
        if reqs.len() == 1 {
            return self.evaluate(spec, &reqs[0], request_id).await;
        }
        let decisions = self
            .evaluate_batch_inner(spec, reqs, BatchSemantic::PermitOnFirstPermit, request_id)
            .await?;
        Ok(decisions.into_iter().any(|d| d))
    }

    async fn evaluate_batch_inner(
        &self,
        spec: &AuthorizerSpec,
        reqs: &[AuthzenRequest],
        semantic: BatchSemantic,
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
                    metrics::record_authorizer_cache(&spec.name, "hit");
                    results[i] = Some(entry.decision);
                    if semantic == BatchSemantic::PermitOnFirstPermit && entry.decision {
                        return Ok(vec![true]);
                    }
                    continue;
                }
                metrics::record_authorizer_cache(&spec.name, "miss");
                misses.push(i);
            }
        } else {
            misses.extend(0..reqs.len());
        }

        if !misses.is_empty() {
            for chunk in misses.chunks(MAX_BATCH_EVALUATIONS) {
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
                                .insert(key, CachedDecision { decision, ttl })
                                .await;
                        }
                    }
                    if semantic == BatchSemantic::PermitOnFirstPermit && decision {
                        return Ok(vec![true]);
                    }
                }
            }
        }

        Ok(results.into_iter().map(|r| r.unwrap_or(false)).collect())
    }
}

fn hash_json_map(
    h: &mut blake3::Hasher,
    tag: &str,
    map: &BTreeMap<String, Value>,
    skip_key: Option<&str>,
) {
    hash_part(h, tag);
    for (k, v) in map {
        if skip_key == Some(k.as_str()) {
            continue;
        }
        hash_part(h, k);
        // `serde_json::to_string` on `Value` is deterministic because object
        // keys use BTreeMap (default serde_json without preserve_order).
        if let Ok(encoded) = serde_json::to_string(v) {
            hash_part(h, &encoded);
        }
    }
}

#[cfg(test)]
mod tests {
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::sync::Arc;

    use serde_json::json;

    use super::*;
    use crate::authorizer::{AuthzenAction, AuthzenResource, AuthzenSubject};

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
            _semantic: BatchSemantic,
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

    fn sample_req(subject_id: &str, project_id: &str, doc_id: &str) -> AuthzenRequest {
        let mut res_props = BTreeMap::new();
        res_props.insert("root_id".into(), json!("01org"));
        res_props.insert("project_id".into(), json!(project_id));
        let mut ctx = BTreeMap::new();
        ctx.insert("request_id".into(), json!("req-unique"));
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
                properties: res_props,
            },
            context: ctx,
        }
    }

    #[tokio::test]
    async fn cache_hits_ignore_request_id_and_separate_parent_nodes() {
        let calls = Arc::new(AtomicUsize::new(0));
        let cache = AuthorizerCache::new(StubPdp {
            calls: calls.clone(),
            decision: Arc::new(std::sync::Mutex::new(true)),
        });
        let spec = sample_spec(60);
        let mut req1 = sample_req("01m1", "01proj-a", "doc-1");
        let mut req2 = sample_req("01m1", "01proj-a", "doc-1");
        req2.context.insert("request_id".into(), json!("req-other"));
        let req_other_proj = sample_req("01m1", "01proj-b", "doc-1");

        assert!(cache.evaluate(&spec, &req1, "r1").await.unwrap());
        assert!(cache.evaluate(&spec, &req2, "r2").await.unwrap());
        assert_eq!(calls.load(Ordering::SeqCst), 1);

        // Same doc_id under a different parent project must not collide.
        assert!(cache.evaluate(&spec, &req_other_proj, "r3").await.unwrap());
        assert_eq!(calls.load(Ordering::SeqCst), 2);

        // Different action method must not collide.
        req1.action
            .properties
            .insert("method".into(), json!("POST"));
        assert!(cache.evaluate(&spec, &req1, "r4").await.unwrap());
        assert_eq!(calls.load(Ordering::SeqCst), 3);
    }

    #[tokio::test]
    async fn zero_ttl_never_caches() {
        let calls = Arc::new(AtomicUsize::new(0));
        let cache = AuthorizerCache::new(StubPdp {
            calls: calls.clone(),
            decision: Arc::new(std::sync::Mutex::new(true)),
        });
        let spec = sample_spec(0);
        let req = sample_req("01m1", "01proj-a", "doc-1");
        assert!(cache.evaluate(&spec, &req, "r1").await.unwrap());
        assert!(cache.evaluate(&spec, &req, "r2").await.unwrap());
        assert_eq!(calls.load(Ordering::SeqCst), 2);
    }
}
