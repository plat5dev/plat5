//! Identity's admit (docs/reference/api/internal.md#admit) and the gateway's cache
//! of its answers (docs/reference/gateway.md#cache).

use std::sync::Arc;
use std::time::{Duration, Instant};

use moka::future::Cache;
use serde::{Deserialize, Serialize};
use std::future::Future;
use tracing::{info, warn};

use crate::auth::call::AuthCallTimer;
use crate::internal_http::InternalHttpClient;
use crate::metrics;

const CACHE_CAPACITY: u64 = 50_000;

/// One admit call: a credential and the route's chain. An empty chain is a user route.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct AdmitRequest {
    #[serde(flatten)]
    pub credential: AdmitCredential,
    pub chain: Vec<ChainRef>,
    pub allow_deleted: bool,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum AdmitCredential {
    /// A bearer value that isn't a JWT, as presented.
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
    #[serde(deserialize_with = "Option::deserialize")]
    pub principal: Option<String>,
    #[serde(deserialize_with = "Option::deserialize")]
    pub key_id: Option<String>,
    pub nodes: Vec<NodeRef>,
    #[serde(deserialize_with = "Option::deserialize")]
    pub member_id: Option<String>,
    #[serde(deserialize_with = "Option::deserialize")]
    pub user_id: Option<String>,
    pub permissions: Vec<String>,
}

impl AdmitAnswer {
    /// A refusal: an invalid credential, or no member on a tree route.
    pub fn refused(&self, req: &AdmitRequest) -> bool {
        !self.valid || (!req.chain.is_empty() && self.member_id.is_none())
    }
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

/// `POST {IDENTITY_INTERNAL_URL}/internal/admit`.
pub struct HttpAdmit {
    url: String,
    http: InternalHttpClient,
}

impl HttpAdmit {
    pub fn new(identity_internal_url: &str, http: InternalHttpClient) -> Self {
        let url = format!("{identity_internal_url}/internal/admit");
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

/// Admit answers by credential (a key as presented, or a JWT's issuer and subject),
/// `allow_deleted` and the chain refs. Refusals live for the negative TTL. Concurrent
/// misses for one key share a single call; an identity error is never cached.
pub struct AdmitCache<A> {
    admit: A,
    ttl: Duration,
    negative_ttl: Duration,
    entries: Cache<String, Entry>,
}

impl<A: Admit> AdmitCache<A> {
    pub fn new(admit: A, ttl: Duration, negative_ttl: Duration) -> Self {
        let entries = Cache::builder()
            .max_capacity(CACHE_CAPACITY)
            .expire_after(EntryExpiry)
            .build();
        Self {
            admit,
            ttl,
            negative_ttl,
            entries,
        }
    }

    pub async fn get(&self, req: &AdmitRequest) -> Result<Arc<AdmitAnswer>, Arc<AdmitUnavailable>> {
        let key = cache_key(req);
        if let Some(entry) = self.entries.get(&key).await {
            metrics::record_auth_cache_hit("admit");
            return Ok(entry.answer);
        }
        metrics::record_auth_cache_miss("admit");
        self.entries
            .try_get_with(key, async {
                let answer = self.admit.admit(req).await?;
                if !answer.refused(req) {
                    check_admitted(&answer, req).map_err(|e| {
                        warn!(error_message = e, "admit answered without a field it needs");
                        AdmitUnavailable(e.to_string())
                    })?;
                }
                let ttl = if answer.refused(req) {
                    self.negative_ttl
                } else {
                    self.ttl
                };
                Ok(Entry {
                    answer: Arc::new(answer),
                    ttl,
                })
            })
            .await
            .map(|entry| entry.answer)
    }
}

/// An admitted answer names the principal, and on a tree route one node per chain
/// ref, by kind; on a user route, the user.
fn check_admitted(answer: &AdmitAnswer, req: &AdmitRequest) -> Result<(), &'static str> {
    if answer.principal.as_deref().is_none_or(str::is_empty) {
        return Err("principal");
    }
    if req.chain.is_empty() {
        return match answer.user_id.as_deref() {
            Some(id) if !id.is_empty() => Ok(()),
            _ => Err("user_id"),
        };
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

/// A hash of the credential, `allow_deleted` and the chain refs as written.
fn cache_key(req: &AdmitRequest) -> String {
    let mut h = blake3::Hasher::new();
    // Each part is length-prefixed so no two requests share a key by concatenation.
    let mut part = |s: &str| {
        h.update(&(s.len() as u64).to_le_bytes());
        h.update(s.as_bytes());
    };
    match &req.credential {
        AdmitCredential::Credential(key) => {
            part("key");
            part(key);
        }
        AdmitCredential::Idp {
            issuer, subject, ..
        } => {
            part("idp");
            part(issuer);
            part(subject);
        }
    }
    part(if req.allow_deleted { "1" } else { "0" });
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

    pub const MEMBER: &str = "01jby0000000000000000member";
    pub const USER: &str = "01jbu00000000000000000user";
    pub const KEY_ID: &str = "01jbz000000000000000000key";

    /// A member on a tree route, admitted by a key.
    pub fn admitted(nodes: &[(&str, &str)], permissions: &[&str]) -> AdmitAnswer {
        AdmitAnswer {
            valid: true,
            principal: Some("person".into()),
            key_id: Some(KEY_ID.into()),
            nodes: nodes
                .iter()
                .map(|(kind, id)| NodeRef {
                    kind: kind.to_string(),
                    id: id.to_string(),
                })
                .collect(),
            member_id: Some(MEMBER.into()),
            user_id: None,
            permissions: permissions.iter().map(|p| p.to_string()).collect(),
        }
    }

    /// A person on a user route.
    pub fn user() -> AdmitAnswer {
        AdmitAnswer {
            valid: true,
            principal: Some("person".into()),
            key_id: None,
            nodes: vec![],
            member_id: None,
            user_id: Some(USER.into()),
            permissions: vec![],
        }
    }

    /// A missing node, or a caller outside the tree.
    pub fn not_in_tree() -> AdmitAnswer {
        AdmitAnswer {
            valid: true,
            principal: Some("person".into()),
            key_id: Some(KEY_ID.into()),
            nodes: vec![],
            member_id: None,
            user_id: None,
            permissions: vec![],
        }
    }

    pub fn invalid() -> AdmitAnswer {
        AdmitAnswer {
            valid: false,
            principal: None,
            key_id: None,
            nodes: vec![],
            member_id: None,
            user_id: None,
            permissions: vec![],
        }
    }

    fn request(
        credential: AdmitCredential,
        refs: &[(&str, &str)],
        allow_deleted: bool,
    ) -> AdmitRequest {
        AdmitRequest {
            credential,
            chain: refs
                .iter()
                .map(|(kind, r)| ChainRef {
                    kind: kind.to_string(),
                    reference: r.to_string(),
                })
                .collect(),
            allow_deleted,
        }
    }

    fn key(k: &str) -> AdmitCredential {
        AdmitCredential::Credential(k.into())
    }

    fn idp(subject: &str, name: Option<&str>) -> AdmitCredential {
        AdmitCredential::Idp {
            issuer: "https://auth".into(),
            subject: subject.into(),
            name: name.map(str::to_string),
        }
    }

    #[test]
    fn wire_shapes() {
        let r = request(
            key("k"),
            &[("organization", "acme"), ("project", "web")],
            true,
        );
        assert_eq!(
            serde_json::to_value(&r).unwrap(),
            serde_json::json!({
                "credential": "k",
                "chain": [{ "kind": "organization", "ref": "acme" }, { "kind": "project", "ref": "web" }],
                "allow_deleted": true
            })
        );
        let jwt = request(idp("s", Some("Dana")), &[], false);
        assert_eq!(
            serde_json::to_value(&jwt).unwrap(),
            serde_json::json!({
                "idp": { "issuer": "https://auth", "subject": "s", "name": "Dana" },
                "chain": [],
                "allow_deleted": false
            })
        );
    }

    #[test]
    fn every_answer_field_is_required() {
        let full = serde_json::json!({
            "valid": true, "principal": "person", "key_id": null, "nodes": [],
            "member_id": null, "user_id": "01u", "permissions": []
        });
        assert!(serde_json::from_value::<AdmitAnswer>(full.clone()).is_ok());
        for field in [
            "valid",
            "principal",
            "key_id",
            "nodes",
            "member_id",
            "user_id",
            "permissions",
        ] {
            let mut v = full.clone();
            v.as_object_mut().unwrap().remove(field);
            assert!(serde_json::from_value::<AdmitAnswer>(v).is_err(), "{field}");
        }
    }

    #[test]
    fn key_covers_credential_allow_deleted_and_refs() {
        let base = request(
            key("secret"),
            &[("organization", "acme"), ("project", "web")],
            false,
        );
        let k = cache_key(&base);
        assert_eq!(k, cache_key(&base.clone()));
        assert_ne!(
            k,
            cache_key(&AdmitRequest {
                credential: key("other"),
                ..base.clone()
            })
        );
        let mut other = base.clone();
        other.allow_deleted = true;
        assert_ne!(
            k,
            cache_key(&other),
            "the restore route must not share a read's entry"
        );
        let by_id = request(
            key("secret"),
            &[("organization", "01jbw"), ("project", "web")],
            false,
        );
        assert_ne!(k, cache_key(&by_id));
        // Parts can't run into each other.
        let a = request(key("s"), &[("organization", "ab"), ("project", "c")], false);
        let b = request(key("s"), &[("organization", "a"), ("project", "bc")], false);
        assert_ne!(cache_key(&a), cache_key(&b));
    }

    #[test]
    fn a_jwt_is_keyed_by_issuer_and_subject() {
        // A refreshed token, or a changed name, is the same entry.
        assert_eq!(
            cache_key(&request(idp("s", Some("Dana")), &[], false)),
            cache_key(&request(idp("s", None), &[], false))
        );
        assert_ne!(
            cache_key(&request(idp("s", None), &[], false)),
            cache_key(&request(idp("t", None), &[], false))
        );
        // A key that happens to spell a subject is another entry.
        assert_ne!(
            cache_key(&request(idp("s", None), &[], false)),
            cache_key(&request(key("s"), &[], false))
        );
    }

    fn cache(answer: Result<AdmitAnswer, String>) -> (AdmitCache<Arc<FakeAdmit>>, Arc<FakeAdmit>) {
        let fake = Arc::new(FakeAdmit::new(answer));
        (
            AdmitCache::new(
                fake.clone(),
                Duration::from_secs(300),
                Duration::from_secs(10),
            ),
            fake,
        )
    }

    #[tokio::test]
    async fn caches_answers_and_refusals() {
        let req = request(key("k"), &[("organization", "acme")], false);
        for answer in [
            admitted(&[("organization", "01o")], &["*"]),
            not_in_tree(),
            invalid(),
        ] {
            let (c, fake) = cache(Ok(answer.clone()));
            assert_eq!(*c.get(&req).await.unwrap(), answer);
            assert_eq!(*c.get(&req).await.unwrap(), answer);
            assert_eq!(fake.calls.load(Ordering::SeqCst), 1);
        }
    }

    #[tokio::test]
    async fn a_refusal_lives_for_the_negative_ttl() {
        let req = request(key("k"), &[("organization", "acme")], false);
        let fake = Arc::new(FakeAdmit::new(Ok(not_in_tree())));
        let c = AdmitCache::new(fake.clone(), Duration::from_secs(300), Duration::ZERO);
        c.get(&req).await.unwrap();
        c.entries.run_pending_tasks().await;
        c.get(&req).await.unwrap();
        assert_eq!(fake.calls.load(Ordering::SeqCst), 2);

        // The same answer to a user route is no refusal: the person has no member there.
        let user_req = request(key("k"), &[], false);
        let fake = Arc::new(FakeAdmit::new(Ok(user())));
        let c = AdmitCache::new(fake.clone(), Duration::from_secs(300), Duration::ZERO);
        c.get(&user_req).await.unwrap();
        c.entries.run_pending_tasks().await;
        c.get(&user_req).await.unwrap();
        assert_eq!(fake.calls.load(Ordering::SeqCst), 1);
    }

    #[tokio::test]
    async fn never_caches_identity_errors() {
        let req = request(key("k"), &[("organization", "acme")], false);
        let (c, fake) = cache(Err("down".into()));
        assert!(c.get(&req).await.is_err());
        assert!(c.get(&req).await.is_err());
        assert_eq!(fake.calls.load(Ordering::SeqCst), 2);
    }

    #[tokio::test]
    async fn an_admitted_answer_missing_a_field_is_an_error() {
        let req = request(
            key("k"),
            &[("organization", "acme"), ("project", "web")],
            false,
        );
        for bad in [
            admitted(&[("organization", "01o")], &[]),
            admitted(&[("organization", "01o"), ("folder", "01f")], &[]),
            AdmitAnswer {
                principal: None,
                ..admitted(&[("organization", "01o"), ("project", "01p")], &[])
            },
        ] {
            let (c, _) = cache(Ok(bad.clone()));
            assert!(c.get(&req).await.is_err(), "{bad:?}");
        }
        let user_req = request(key("k"), &[], false);
        let (c, _) = cache(Ok(AdmitAnswer {
            user_id: None,
            ..user()
        }));
        assert!(c.get(&user_req).await.is_err());
    }

    #[tokio::test]
    async fn concurrent_misses_share_one_call() {
        let req = request(key("k"), &[("organization", "acme")], false);
        let fake = Arc::new(FakeAdmit {
            delay: Duration::from_millis(50),
            ..FakeAdmit::new(Ok(admitted(&[("organization", "01o")], &[])))
        });
        let c = Arc::new(AdmitCache::new(
            fake.clone(),
            Duration::from_secs(300),
            Duration::from_secs(10),
        ));
        let mut tasks = Vec::new();
        for _ in 0..16 {
            let (c, req) = (c.clone(), req.clone());
            tasks.push(tokio::spawn(async move { c.get(&req).await.is_ok() }));
        }
        for t in tasks {
            assert!(t.await.unwrap());
        }
        assert_eq!(fake.calls.load(Ordering::SeqCst), 1);
    }
}
