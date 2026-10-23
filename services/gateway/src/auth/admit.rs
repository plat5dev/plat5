//! Identity's admit (docs/reference/api/internal.md#admit) and the gateway's cache
//! of its answers (docs/reference/gateway.md#cache).

use std::sync::Arc;
use std::time::{Duration, Instant, SystemTime};

use moka::future::Cache;
use serde::{Deserialize, Serialize};
use std::future::Future;
use tracing::{info, warn};

use crate::auth::call::AuthCallTimer;
use crate::clock::parse_rfc3339;
use crate::internal_http::InternalHttpClient;
use crate::metrics;

const CACHE_CAPACITY: u64 = 50_000;

/// One admit call: a credential and the route's chain.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct AdmitRequest {
    #[serde(flatten)]
    pub credential: AdmitCredential,
    pub scope: String,
    pub chain: Vec<ChainRef>,
    pub resolve_deleted: bool,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum AdmitCredential {
    /// An `X-API-Key` value.
    Credential(String),
    /// A verified JWT's issuer, subject and name claim.
    Idp {
        issuer: String,
        subject: String,
        #[serde(skip_serializing_if = "Option::is_none")]
        name: Option<String>,
    },
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct ChainRef {
    pub kind: String,
    /// A slug or an id, as written in the path.
    #[serde(rename = "ref")]
    pub reference: String,
}

#[derive(Clone, Debug, PartialEq, Eq, Deserialize, Serialize)]
pub struct NodeRef {
    pub kind: String,
    pub id: String,
}

/// Admit's answer. Every field is in every answer; one missing is identity failing.
#[derive(Clone, Debug, PartialEq, Eq, Deserialize)]
pub struct AdmitAnswer {
    pub valid: bool,
    pub found: bool,
    #[serde(deserialize_with = "Option::deserialize")]
    pub principal: Option<String>,
    pub nodes: Vec<NodeRef>,
    #[serde(deserialize_with = "Option::deserialize")]
    pub member_id: Option<String>,
    pub labels: Vec<String>,
    #[serde(deserialize_with = "Option::deserialize")]
    pub good_until: Option<String>,
}

/// Identity didn't answer usably: transport, non-2xx, or a malformed body. **503**.
#[derive(Debug)]
pub struct AdmitUnavailable(pub String);

pub trait Admit: Send + Sync {
    fn admit(
        &self,
        req: &AdmitRequest,
    ) -> impl Future<Output = Result<AdmitAnswer, AdmitUnavailable>> + Send;
}

/// `POST {ADMIT_URL}` on identity's internal port.
pub struct HttpAdmit {
    url: String,
    http: InternalHttpClient,
}

impl HttpAdmit {
    pub fn new(url: String, http: InternalHttpClient) -> Self {
        info!(admit_url = %url, "admit client initialized");
        Self { url, http }
    }
}

impl Admit for HttpAdmit {
    async fn admit(&self, req: &AdmitRequest) -> Result<AdmitAnswer, AdmitUnavailable> {
        let timer = AuthCallTimer::start("admit");
        match self.http.post_json::<_, AdmitAnswer>(&self.url, req).await {
            Ok(answer) => {
                timer.finish(if answer.valid { "ok" } else { "invalid" });
                Ok(answer)
            }
            Err(err) => Err(AdmitUnavailable(timer.finish_transport(err, "admit"))),
        }
    }
}

#[derive(Clone)]
struct Entry {
    answer: Arc<AdmitAnswer>,
    ttl: Duration,
}

/// Admit answers by credential, scope, `resolve_deleted` and chain refs. Each entry
/// lives for the lesser of the TTL and the answer's `good_until`. Concurrent misses
/// for one key share a single call; an identity error is never cached.
pub struct AdmitCache<A> {
    admit: A,
    ttl: Duration,
    entries: Cache<String, Entry>,
}

impl<A: Admit> AdmitCache<A> {
    pub fn new(admit: A, ttl: Duration) -> Self {
        let entries = Cache::builder()
            .max_capacity(CACHE_CAPACITY)
            .expire_after(EntryExpiry)
            .build();
        Self {
            admit,
            ttl,
            entries,
        }
    }

    /// The answer for `req`. `secret` is the raw key or JWT: hashed into the key,
    /// never kept.
    pub async fn get(
        &self,
        secret: &str,
        req: &AdmitRequest,
    ) -> Result<Arc<AdmitAnswer>, Arc<AdmitUnavailable>> {
        let key = cache_key(secret, req);
        if let Some(entry) = self.entries.get(&key).await {
            metrics::record_auth_cache_hit("admit");
            return Ok(entry.answer);
        }
        metrics::record_auth_cache_miss("admit");
        self.entries
            .try_get_with(key, async {
                let answer = self.admit.admit(req).await?;
                if answer.valid && answer.found && answer.member_id.is_some() {
                    check_admitted(&answer, req).map_err(|e| {
                        warn!(error_message = e, "admit answered without a field it needs");
                        AdmitUnavailable(e.to_string())
                    })?;
                }
                let ttl = ttl_for(&answer, self.ttl, SystemTime::now()).map_err(|e| {
                    warn!(error_message = e, "admit answered a malformed good_until");
                    AdmitUnavailable(e.to_string())
                })?;
                Ok(Entry {
                    answer: Arc::new(answer),
                    ttl,
                })
            })
            .await
            .map(|entry| entry.answer)
    }
}

/// An admitted answer names the principal and one node per chain ref, by kind.
fn check_admitted(answer: &AdmitAnswer, req: &AdmitRequest) -> Result<(), &'static str> {
    if answer.principal.as_deref().is_none_or(str::is_empty) {
        return Err("principal");
    }
    if answer.member_id.as_deref().is_some_and(str::is_empty) {
        return Err("member_id");
    }
    let kinds_match = answer.nodes.len() == req.chain.len()
        && answer
            .nodes
            .iter()
            .zip(&req.chain)
            .all(|(node, link)| node.kind == link.kind && !node.id.is_empty());
    if !kinds_match {
        return Err("nodes");
    }
    Ok(())
}

/// The lesser of `ttl` and the time left until `good_until`. Zero = don't keep.
fn ttl_for(answer: &AdmitAnswer, ttl: Duration, now: SystemTime) -> Result<Duration, &'static str> {
    let Some(raw) = answer.good_until.as_deref() else {
        return Ok(ttl);
    };
    let until = parse_rfc3339(raw).ok_or("good_until")?;
    Ok(until.duration_since(now).unwrap_or(Duration::ZERO).min(ttl))
}

/// A hash of the credential, the scope, `resolve_deleted` and the chain refs as written.
fn cache_key(secret: &str, req: &AdmitRequest) -> String {
    let mut h = blake3::Hasher::new();
    // Each part is length-prefixed so no two requests share a key by concatenation.
    let mut part = |s: &str| {
        h.update(&(s.len() as u64).to_le_bytes());
        h.update(s.as_bytes());
    };
    part(secret);
    part(&req.scope);
    part(if req.resolve_deleted { "1" } else { "0" });
    for link in &req.chain {
        part(&link.kind);
        part(&link.reference);
    }
    h.finalize().to_hex().to_string()
}

struct EntryExpiry;

impl moka::Expiry<String, Entry> for EntryExpiry {
    fn expire_after_create(
        &self,
        _key: &String,
        value: &Entry,
        _created_at: Instant,
    ) -> Option<Duration> {
        Some(value.ttl)
    }
}

#[cfg(test)]
pub mod tests {
    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::sync::Mutex;

    /// A fake identity: answers from `answer`, counting calls and keeping the last request.
    pub struct FakeAdmit {
        pub answer: Mutex<Result<AdmitAnswer, String>>,
        pub calls: AtomicUsize,
        pub last: Mutex<Option<AdmitRequest>>,
        pub delay: Duration,
    }

    impl FakeAdmit {
        pub fn new(answer: Result<AdmitAnswer, String>) -> Self {
            Self {
                answer: Mutex::new(answer),
                calls: AtomicUsize::new(0),
                last: Mutex::new(None),
                delay: Duration::ZERO,
            }
        }
    }

    impl Admit for Arc<FakeAdmit> {
        async fn admit(&self, req: &AdmitRequest) -> Result<AdmitAnswer, AdmitUnavailable> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            *self.last.lock().unwrap() = Some(req.clone());
            if !self.delay.is_zero() {
                tokio::time::sleep(self.delay).await;
            }
            self.answer
                .lock()
                .unwrap()
                .clone()
                .map_err(AdmitUnavailable)
        }
    }

    pub fn admitted(nodes: &[(&str, &str)], labels: &[&str]) -> AdmitAnswer {
        AdmitAnswer {
            valid: true,
            found: true,
            principal: Some("person".into()),
            nodes: nodes
                .iter()
                .map(|(kind, id)| NodeRef {
                    kind: kind.to_string(),
                    id: id.to_string(),
                })
                .collect(),
            member_id: Some("m_1".into()),
            labels: labels.iter().map(|l| l.to_string()).collect(),
            good_until: None,
        }
    }

    pub fn not_found() -> AdmitAnswer {
        AdmitAnswer {
            valid: true,
            found: false,
            principal: Some("person".into()),
            nodes: vec![],
            member_id: None,
            labels: vec![],
            good_until: None,
        }
    }

    pub fn invalid() -> AdmitAnswer {
        AdmitAnswer {
            valid: false,
            found: false,
            principal: None,
            nodes: vec![],
            member_id: None,
            labels: vec![],
            good_until: None,
        }
    }

    fn request(scope: &str, refs: &[(&str, &str)], resolve_deleted: bool) -> AdmitRequest {
        AdmitRequest {
            credential: AdmitCredential::Credential("k".into()),
            scope: scope.into(),
            chain: refs
                .iter()
                .map(|(kind, r)| ChainRef {
                    kind: kind.to_string(),
                    reference: r.to_string(),
                })
                .collect(),
            resolve_deleted,
        }
    }

    #[test]
    fn wire_shapes() {
        let key = request(
            "project",
            &[("organization", "acme"), ("project", "web")],
            true,
        );
        assert_eq!(
            serde_json::to_value(&key).unwrap(),
            serde_json::json!({
                "credential": "k", "scope": "project",
                "chain": [{ "kind": "organization", "ref": "acme" }, { "kind": "project", "ref": "web" }],
                "resolve_deleted": true
            })
        );
        let jwt = AdmitRequest {
            credential: AdmitCredential::Idp {
                issuer: "https://auth".into(),
                subject: "s".into(),
                name: Some("Dana".into()),
            },
            ..request("directory", &[], false)
        };
        assert_eq!(
            serde_json::to_value(&jwt).unwrap()["idp"],
            serde_json::json!({ "issuer": "https://auth", "subject": "s", "name": "Dana" })
        );
    }

    #[test]
    fn every_answer_field_is_required() {
        let full = serde_json::json!({
            "valid": true, "found": true, "principal": "person", "nodes": [],
            "member_id": "m_1", "labels": [], "good_until": null
        });
        assert!(serde_json::from_value::<AdmitAnswer>(full.clone()).is_ok());
        for field in [
            "valid",
            "found",
            "principal",
            "nodes",
            "member_id",
            "labels",
            "good_until",
        ] {
            let mut v = full.clone();
            v.as_object_mut().unwrap().remove(field);
            assert!(serde_json::from_value::<AdmitAnswer>(v).is_err(), "{field}");
        }
    }

    #[test]
    fn key_covers_credential_scope_resolve_deleted_and_refs() {
        let base = request(
            "project",
            &[("organization", "acme"), ("project", "web")],
            false,
        );
        let k = cache_key("secret", &base);
        assert_eq!(k, cache_key("secret", &base.clone()));
        assert_ne!(k, cache_key("other", &base));
        let mut other = base.clone();
        other.resolve_deleted = true;
        assert_ne!(
            k,
            cache_key("secret", &other),
            "the restore route must not share a read's entry"
        );
        let mut other = base.clone();
        other.scope = "organization".into();
        assert_ne!(k, cache_key("secret", &other));
        let by_id = request(
            "project",
            &[("organization", "n_1"), ("project", "web")],
            false,
        );
        assert_ne!(k, cache_key("secret", &by_id));
        // Parts can't run into each other.
        let a = request(
            "project",
            &[("organization", "ab"), ("project", "c")],
            false,
        );
        let b = request(
            "project",
            &[("organization", "a"), ("project", "bc")],
            false,
        );
        assert_ne!(cache_key("s", &a), cache_key("s", &b));
    }

    #[test]
    fn ttl_is_the_lesser_of_the_setting_and_good_until() {
        let now = parse_rfc3339("2026-10-01T00:00:00Z").unwrap();
        let ttl = Duration::from_secs(300);
        let mut a = admitted(&[], &[]);
        assert_eq!(ttl_for(&a, ttl, now), Ok(ttl));
        a.good_until = Some("2026-10-01T00:01:00Z".into());
        assert_eq!(ttl_for(&a, ttl, now), Ok(Duration::from_secs(60)));
        a.good_until = Some("2026-10-02T00:00:00Z".into());
        assert_eq!(ttl_for(&a, ttl, now), Ok(ttl));
        a.good_until = Some("2026-09-30T00:00:00Z".into());
        assert_eq!(ttl_for(&a, ttl, now), Ok(Duration::ZERO));
        a.good_until = Some("soon".into());
        assert!(ttl_for(&a, ttl, now).is_err());
    }

    fn cache(answer: Result<AdmitAnswer, String>) -> (AdmitCache<Arc<FakeAdmit>>, Arc<FakeAdmit>) {
        let fake = Arc::new(FakeAdmit::new(answer));
        (
            AdmitCache::new(fake.clone(), Duration::from_secs(300)),
            fake,
        )
    }

    #[tokio::test]
    async fn caches_answers_and_negative_answers() {
        let req = request("organization", &[("organization", "acme")], false);
        for answer in [
            admitted(&[("organization", "n_1")], &["*"]),
            not_found(),
            invalid(),
        ] {
            let (c, fake) = cache(Ok(answer.clone()));
            assert_eq!(*c.get("k", &req).await.unwrap(), answer);
            assert_eq!(*c.get("k", &req).await.unwrap(), answer);
            assert_eq!(fake.calls.load(Ordering::SeqCst), 1);
        }
    }

    #[tokio::test]
    async fn never_caches_identity_errors() {
        let req = request("organization", &[("organization", "acme")], false);
        let (c, fake) = cache(Err("down".into()));
        assert!(c.get("k", &req).await.is_err());
        assert!(c.get("k", &req).await.is_err());
        assert_eq!(fake.calls.load(Ordering::SeqCst), 2);
    }

    #[tokio::test]
    async fn an_answer_past_good_until_is_not_kept() {
        let req = request("organization", &[("organization", "acme")], false);
        let mut a = admitted(&[("organization", "n_1")], &["x"]);
        a.good_until = Some("2000-01-01T00:00:00Z".into());
        let (c, fake) = cache(Ok(a));
        c.get("k", &req).await.unwrap();
        c.entries.run_pending_tasks().await;
        c.get("k", &req).await.unwrap();
        assert_eq!(fake.calls.load(Ordering::SeqCst), 2);
    }

    #[tokio::test]
    async fn an_admitted_answer_missing_a_node_is_an_error() {
        let req = request(
            "project",
            &[("organization", "acme"), ("project", "web")],
            false,
        );
        for bad in [
            admitted(&[("organization", "n_1")], &[]),
            admitted(&[("organization", "n_1"), ("folder", "n_2")], &[]),
            AdmitAnswer {
                principal: None,
                ..admitted(&[("organization", "n_1"), ("project", "n_2")], &[])
            },
        ] {
            let (c, _) = cache(Ok(bad.clone()));
            assert!(c.get("k", &req).await.is_err(), "{bad:?}");
        }
    }

    #[tokio::test]
    async fn concurrent_misses_share_one_call() {
        let req = request("organization", &[("organization", "acme")], false);
        let fake = Arc::new(FakeAdmit {
            delay: Duration::from_millis(50),
            ..FakeAdmit::new(Ok(admitted(&[("organization", "n_1")], &[])))
        });
        let c = Arc::new(AdmitCache::new(fake.clone(), Duration::from_secs(300)));
        let mut tasks = Vec::new();
        for _ in 0..16 {
            let (c, req) = (c.clone(), req.clone());
            tasks.push(tokio::spawn(async move { c.get("k", &req).await.is_ok() }));
        }
        for t in tasks {
            assert!(t.await.unwrap());
        }
        assert_eq!(fake.calls.load(Ordering::SeqCst), 1);
    }
}
