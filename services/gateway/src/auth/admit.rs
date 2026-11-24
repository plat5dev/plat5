//! Identity's admit (docs/reference/api/internal.md#admit) and the gateway's cache
//! of its answers (docs/reference/gateway.md#cache).

use std::net::IpAddr;
use std::sync::Arc;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use moka::future::Cache;
use moka::ops::compute::{CompResult, Op};
use serde::{Deserialize, Serialize};
use std::future::Future;
use tracing::{info, warn};

use crate::auth::call::AuthCallTimer;
use crate::auth::invalidations::InvalidationTracker;
use crate::client_ip::Cidr;
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
    /// A verified JWT's issuer, subject, optional name and optional OAuth scopes.
    Idp {
        issuer: String,
        subject: String,
        #[serde(skip_serializing_if = "Option::is_none")]
        name: Option<String>,
        #[serde(skip_serializing_if = "Option::is_none")]
        scopes: Option<Vec<String>>,
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
    pub allowed_cidrs: Vec<String>,
    pub nodes: Vec<NodeRef>,
    #[serde(deserialize_with = "Option::deserialize")]
    pub member_id: Option<String>,
    #[serde(deserialize_with = "Option::deserialize")]
    pub user_id: Option<String>,
    pub permissions: Vec<String>,
    #[serde(deserialize_with = "Option::deserialize")]
    pub rate_limit_tier: Option<String>,
    #[serde(deserialize_with = "Option::deserialize")]
    pub expires_at: Option<String>,
    pub invalidation_seq: i64,
    pub config_revision: i64,
}

impl AdmitAnswer {
    /// A refusal: an invalid credential, or no member on a tree route.
    pub fn refused(&self, req: &AdmitRequest) -> bool {
        !self.valid || (!req.chain.is_empty() && self.member_id.is_none())
    }

    /// Whether the client IP is permitted by the key's IP allowlist.
    /// An empty allowlist permits any IP.
    pub fn allows_ip(&self, ip: &str) -> bool {
        if self.allowed_cidrs.is_empty() {
            return true;
        }
        let Ok(addr) = ip.parse::<IpAddr>() else {
            return false;
        };
        self.allowed_cidrs
            .iter()
            .any(|raw| Cidr::parse(raw).is_ok_and(|c| c.contains(addr)))
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
    global_gen: u64,
    seq: i64,
    config_revision: i64,
    is_negative: bool,
}

enum ComputeErr {
    Unavailable(AdmitUnavailable),
    /// Hit a lagging identity replica whose `config_revision` is behind what we
    /// already know: return the answer to the caller without caching it.
    Uncached(Arc<AdmitAnswer>),
}

/// Admit answers by credential (a key as presented, or a JWT's issuer and subject),
/// `allow_deleted` and the chain refs. Refusals live for the negative TTL. Concurrent
/// misses for one key share a single call; an identity error is never cached.
/// Positive entries are checked against [`InvalidationTracker`] epochs on every
/// lookup so revocations and role applies take effect without waiting for TTL.
pub struct AdmitCache<A> {
    admit: A,
    ttl: Duration,
    negative_ttl: Duration,
    entries: Cache<String, Entry>,
    tracker: InvalidationTracker,
}

impl<A: Admit> AdmitCache<A> {
    pub fn new(admit: A, ttl: Duration, negative_ttl: Duration) -> Self {
        Self::with_tracker(admit, ttl, negative_ttl, InvalidationTracker::new(ttl))
    }

    pub fn with_tracker(
        admit: A,
        ttl: Duration,
        negative_ttl: Duration,
        tracker: InvalidationTracker,
    ) -> Self {
        let entries = Cache::builder()
            .max_capacity(CACHE_CAPACITY)
            .expire_after(EntryExpiry)
            .build();
        Self {
            admit,
            ttl,
            negative_ttl,
            entries,
            tracker,
        }
    }

    pub fn tracker(&self) -> &InvalidationTracker {
        &self.tracker
    }

    async fn entry_is_fresh(&self, entry: &Entry) -> bool {
        if entry.is_negative {
            return true;
        }
        self.tracker
            .is_fresh(
                &entry.answer,
                entry.global_gen,
                entry.seq,
                entry.config_revision,
            )
            .await
    }

    pub async fn get(&self, req: &AdmitRequest) -> Result<Arc<AdmitAnswer>, Arc<AdmitUnavailable>> {
        let key = cache_key(req);
        if let Some(entry) = self.entries.get(&key).await {
            if self.entry_is_fresh(&entry).await {
                metrics::record_auth_cache_hit("admit");
                return Ok(entry.answer);
            }
        }
        metrics::record_auth_cache_miss("admit");
        let computed = self
            .entries
            .entry(key)
            .and_try_compute_with(|maybe_entry| async move {
                if let Some(existing) = maybe_entry {
                    if self.entry_is_fresh(existing.value()).await {
                        return Ok(Op::Nop);
                    }
                }
                let stamp = self.tracker.snapshot_before_admit();
                let answer = self
                    .admit
                    .admit(req)
                    .await
                    .map_err(ComputeErr::Unavailable)?;
                if answer.valid && answer.allowed_cidrs.iter().any(|c| Cidr::parse(c).is_err()) {
                    warn!(
                        error_message = "allowed_cidrs",
                        "admit answered with an invalid CIDR"
                    );
                    return Err(ComputeErr::Unavailable(AdmitUnavailable(
                        "allowed_cidrs".to_string(),
                    )));
                }
                let is_negative = answer.refused(req);
                if !is_negative {
                    check_admitted(&answer, req).map_err(|e| {
                        warn!(error_message = e, "admit answered without a field it needs");
                        ComputeErr::Unavailable(AdmitUnavailable(e.to_string()))
                    })?;
                }
                let min_rev = self.tracker.observe_config_revision(answer.config_revision);
                if !is_negative && answer.config_revision < min_rev {
                    return Err(ComputeErr::Uncached(Arc::new(answer)));
                }
                let ttl = if is_negative {
                    self.negative_ttl
                } else if let Some(raw) = answer.expires_at.as_deref() {
                    let Some(exp) = parse_rfc3339_utc(raw) else {
                        warn!(
                            error_message = "expires_at",
                            "admit answered with an invalid expires_at"
                        );
                        return Err(ComputeErr::Unavailable(AdmitUnavailable(
                            "expires_at".to_string(),
                        )));
                    };
                    match exp.duration_since(SystemTime::now()) {
                        Ok(remaining) if !remaining.is_zero() => self.ttl.min(remaining),
                        _ => return Err(ComputeErr::Uncached(Arc::new(answer))),
                    }
                } else {
                    self.ttl
                };
                let seq = self.tracker.entry_seq(stamp, answer.invalidation_seq);
                let config_revision = answer.config_revision;
                Ok(Op::Put(Entry {
                    answer: Arc::new(answer),
                    ttl,
                    global_gen: stamp.global_gen,
                    seq,
                    config_revision,
                    is_negative,
                }))
            })
            .await;

        match computed {
            Ok(CompResult::Inserted(entry))
            | Ok(CompResult::ReplacedWith(entry))
            | Ok(CompResult::Unchanged(entry)) => Ok(entry.into_value().answer),
            Ok(CompResult::StillNone(_)) | Ok(CompResult::Removed(_)) => Err(Arc::new(
                AdmitUnavailable("cache entry removed".to_string()),
            )),
            Err(ComputeErr::Uncached(answer)) => Ok(answer),
            Err(ComputeErr::Unavailable(err)) => Err(Arc::new(err)),
        }
    }
}

/// An admitted answer names the principal, and on a tree route one node per chain
/// ref, by kind; on a user route, a person's user. A service account has no
/// user, and the gateway refuses it there.
fn check_admitted(answer: &AdmitAnswer, req: &AdmitRequest) -> Result<(), &'static str> {
    let Some(principal) = answer.principal.as_deref().filter(|p| !p.is_empty()) else {
        return Err("principal");
    };
    if req.chain.is_empty() {
        return match answer.user_id.as_deref() {
            _ if principal == "service_account" => Ok(()),
            // A narrowed personal API key on a user route returns user_id: null;
            // the pipeline refuses it with 403 rather than treating it as 503.
            None if answer.key_id.is_some() => Ok(()),
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

/// Each part is length-prefixed so no two inputs share a hash by concatenation.
fn hash_part(h: &mut blake3::Hasher, s: &str) {
    h.update(&(s.len() as u64).to_le_bytes());
    h.update(s.as_bytes());
}

/// The credential as the cache keys it: a key as presented, or a JWT's issuer,
/// subject and forwarded scopes.
fn hash_credential(h: &mut blake3::Hasher, credential: &AdmitCredential) {
    match credential {
        AdmitCredential::Credential(key) => {
            hash_part(h, "key");
            hash_part(h, key);
        }
        AdmitCredential::Idp {
            issuer,
            subject,
            scopes,
            ..
        } => {
            hash_part(h, "idp");
            hash_part(h, issuer);
            hash_part(h, subject);
            if let Some(scopes) = scopes {
                hash_part(h, "scopes");
                hash_part(h, &scopes.len().to_string());
                for scope in scopes {
                    hash_part(h, scope);
                }
            }
        }
    }
}

/// Parse an RFC 3339 timestamp (`YYYY-MM-DDTHH:MM:SS[.frac](Z|±HH:MM)`) into [`SystemTime`].
fn parse_rfc3339_utc(s: &str) -> Option<SystemTime> {
    let bytes = s.as_bytes();
    if bytes.len() < 20 {
        return None;
    }
    if bytes[4] != b'-'
        || bytes[7] != b'-'
        || !(bytes[10] == b'T' || bytes[10] == b't' || bytes[10] == b' ')
        || bytes[13] != b':'
        || bytes[16] != b':'
    {
        return None;
    }
    let year: i64 = s[0..4].parse().ok()?;
    let month: u32 = s[5..7].parse().ok()?;
    let day: u32 = s[8..10].parse().ok()?;
    let hour: u32 = s[11..13].parse().ok()?;
    let minute: u32 = s[14..16].parse().ok()?;
    let second: u32 = s[17..19].parse().ok()?;
    if !(1..=12).contains(&month)
        || !(1..=31).contains(&day)
        || hour > 23
        || minute > 59
        || second > 59
    {
        return None;
    }
    if day > days_in_month(year, month) {
        return None;
    }

    let mut rest = &s[19..];
    let mut nanos: u32 = 0;
    if let Some(after_dot) = rest.strip_prefix('.') {
        let frac_len = after_dot.bytes().take_while(|b| b.is_ascii_digit()).count();
        if frac_len == 0 {
            return None;
        }
        let frac_str = &after_dot[..frac_len.min(9)];
        let mut scale = 1_000_000_000u32;
        for b in frac_str.bytes() {
            scale /= 10;
            nanos += u32::from(b - b'0') * scale;
        }
        rest = &after_dot[frac_len..];
    }

    let offset_secs: i64 = if rest == "Z" || rest == "z" {
        0
    } else if rest.len() == 6
        && (rest.starts_with('+') || rest.starts_with('-'))
        && rest.as_bytes()[3] == b':'
    {
        let sign: i64 = if rest.starts_with('+') { 1 } else { -1 };
        let oh: i64 = rest[1..3].parse().ok()?;
        let om: i64 = rest[4..6].parse().ok()?;
        if oh > 23 || om > 59 {
            return None;
        }
        sign * (oh * 3600 + om * 60)
    } else {
        return None;
    };

    let days = days_from_civil(year, month, day);
    let unix_secs = days
        .checked_mul(86_400)?
        .checked_add(i64::from(hour) * 3600 + i64::from(minute) * 60 + i64::from(second))?
        .checked_sub(offset_secs)?;
    if unix_secs >= 0 {
        UNIX_EPOCH.checked_add(Duration::new(unix_secs as u64, nanos))
    } else {
        let abs = unix_secs.checked_neg()? as u64;
        if nanos == 0 {
            UNIX_EPOCH.checked_sub(Duration::from_secs(abs))
        } else {
            UNIX_EPOCH.checked_sub(Duration::new(abs - 1, 1_000_000_000 - nanos))
        }
    }
}

fn is_leap_year(year: i64) -> bool {
    (year % 4 == 0 && year % 100 != 0) || (year % 400 == 0)
}

fn days_in_month(year: i64, month: u32) -> u32 {
    match month {
        1 | 3 | 5 | 7 | 8 | 10 | 12 => 31,
        4 | 6 | 9 | 11 => 30,
        2 => {
            if is_leap_year(year) {
                29
            } else {
                28
            }
        }
        _ => 0,
    }
}

/// Howard Hinnant's civil-to-days algorithm (days since 1970-01-01).
fn days_from_civil(year: i64, month: u32, day: u32) -> i64 {
    let y = year - i64::from(month <= 2);
    let era = y.div_euclid(400);
    let yoe = (y - era * 400) as u64;
    let m = u64::from(month);
    let d = u64::from(day);
    let doy = (153 * (if m > 2 { m - 3 } else { m + 9 }) + 2) / 5 + d - 1;
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    era * 146_097 + doe as i64 - 719_468
}

/// A hash of the credential alone, as the cache keys it: one per key, one per
/// JWT issuer and subject. Never the secret itself.
pub fn credential_hash(credential: &AdmitCredential) -> String {
    let mut h = blake3::Hasher::new();
    hash_credential(&mut h, credential);
    h.finalize().to_hex().to_string()
}

/// A hash of the credential, `allow_deleted` and the chain refs as written.
fn cache_key(req: &AdmitRequest) -> String {
    let mut h = blake3::Hasher::new();
    hash_credential(&mut h, &req.credential);
    hash_part(&mut h, if req.allow_deleted { "1" } else { "0" });
    for link in &req.chain {
        hash_part(&mut h, &link.kind);
        hash_part(&mut h, &link.reference);
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
            allowed_cidrs: vec![],
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
            rate_limit_tier: None,
            expires_at: None,
            invalidation_seq: 0,
            config_revision: 0,
        }
    }

    /// A person on a user route.
    pub fn user() -> AdmitAnswer {
        AdmitAnswer {
            valid: true,
            principal: Some("person".into()),
            key_id: None,
            allowed_cidrs: vec![],
            nodes: vec![],
            member_id: None,
            user_id: Some(USER.into()),
            permissions: vec![],
            rate_limit_tier: None,
            expires_at: None,
            invalidation_seq: 0,
            config_revision: 0,
        }
    }

    /// A service account's key on a user route: valid, with no user.
    pub fn service_account_user() -> AdmitAnswer {
        AdmitAnswer {
            valid: true,
            principal: Some("service_account".into()),
            key_id: Some(KEY_ID.into()),
            allowed_cidrs: vec![],
            nodes: vec![],
            member_id: None,
            user_id: None,
            permissions: vec![],
            rate_limit_tier: None,
            expires_at: None,
            invalidation_seq: 0,
            config_revision: 0,
        }
    }

    /// A narrowed (scoped or root-restricted) personal API key on a user route.
    pub fn scoped_personal_user() -> AdmitAnswer {
        AdmitAnswer {
            valid: true,
            principal: Some("person".into()),
            key_id: Some(KEY_ID.into()),
            allowed_cidrs: vec![],
            nodes: vec![],
            member_id: None,
            user_id: None,
            permissions: vec![],
            rate_limit_tier: None,
            expires_at: None,
            invalidation_seq: 0,
            config_revision: 0,
        }
    }

    /// A missing node, or a caller outside the tree.
    pub fn not_in_tree() -> AdmitAnswer {
        AdmitAnswer {
            valid: true,
            principal: Some("person".into()),
            key_id: Some(KEY_ID.into()),
            allowed_cidrs: vec![],
            nodes: vec![],
            member_id: None,
            user_id: None,
            permissions: vec![],
            rate_limit_tier: None,
            expires_at: None,
            invalidation_seq: 0,
            config_revision: 0,
        }
    }

    pub fn invalid() -> AdmitAnswer {
        AdmitAnswer {
            valid: false,
            principal: None,
            key_id: None,
            allowed_cidrs: vec![],
            nodes: vec![],
            member_id: None,
            user_id: None,
            permissions: vec![],
            rate_limit_tier: None,
            expires_at: None,
            invalidation_seq: 0,
            config_revision: 0,
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
            scopes: None,
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
            "valid": true, "principal": "person", "key_id": null, "allowed_cidrs": [], "nodes": [],
            "member_id": null, "user_id": "01u", "permissions": [],
            "rate_limit_tier": null, "expires_at": null,
            "invalidation_seq": 0, "config_revision": 1
        });
        assert!(serde_json::from_value::<AdmitAnswer>(full.clone()).is_ok());
        for field in [
            "valid",
            "principal",
            "key_id",
            "allowed_cidrs",
            "nodes",
            "member_id",
            "user_id",
            "permissions",
            "rate_limit_tier",
            "expires_at",
            "invalidation_seq",
            "config_revision",
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
        // Different forwarded scopes must not share a cache entry.
        let scoped_a = AdmitCredential::Idp {
            issuer: "https://auth".into(),
            subject: "s".into(),
            name: None,
            scopes: Some(vec!["project:read".into()]),
        };
        let scoped_b = AdmitCredential::Idp {
            issuer: "https://auth".into(),
            subject: "s".into(),
            name: None,
            scopes: Some(vec!["project:write".into()]),
        };
        let scoped_empty = AdmitCredential::Idp {
            issuer: "https://auth".into(),
            subject: "s".into(),
            name: None,
            scopes: Some(vec![]),
        };
        assert_ne!(
            cache_key(&request(idp("s", None), &[], false)),
            cache_key(&request(scoped_a.clone(), &[], false))
        );
        assert_ne!(
            cache_key(&request(scoped_a, &[], false)),
            cache_key(&request(scoped_b, &[], false))
        );
        assert_ne!(
            cache_key(&request(idp("s", None), &[], false)),
            cache_key(&request(scoped_empty, &[], false))
        );
    }

    #[test]
    fn parse_rfc3339_utc_handles_z_fractions_and_offsets() {
        assert_eq!(parse_rfc3339_utc("1970-01-01T00:00:00Z"), Some(UNIX_EPOCH));
        assert_eq!(
            parse_rfc3339_utc("2024-02-29T12:30:45.5Z"),
            Some(UNIX_EPOCH + Duration::new(1_709_209_845, 500_000_000))
        );
        assert_eq!(
            parse_rfc3339_utc("2024-02-29T14:30:45+02:00"),
            Some(UNIX_EPOCH + Duration::from_secs(1_709_209_845))
        );
        // Non-leap year Feb 29 and garbage timestamps are rejected.
        assert!(parse_rfc3339_utc("2023-02-29T00:00:00Z").is_none());
        assert!(parse_rfc3339_utc("not-a-timestamp").is_none());
        assert!(parse_rfc3339_utc("2026-01-01T00:00:00").is_none());
    }

    #[tokio::test]
    async fn expires_at_clamps_ttl_and_rejects_bad_timestamp() {
        let req = request(key("k"), &[("organization", "acme")], false);

        // Already-expired answer is returned uncached (every call hits identity).
        let mut expired = admitted(&[("organization", "01o")], &["*"]);
        expired.expires_at = Some("2000-01-01T00:00:00Z".into());
        let (c, fake) = cache(Ok(expired));
        assert!(c.get(&req).await.is_ok());
        assert!(c.get(&req).await.is_ok());
        assert_eq!(fake.calls.load(Ordering::SeqCst), 2);

        // Unparseable expires_at is rejected (503) and not cached.
        let mut bad = admitted(&[("organization", "01o")], &["*"]);
        bad.expires_at = Some("tomorrow".into());
        let (c, fake) = cache(Ok(bad));
        assert!(c.get(&req).await.is_err());
        assert!(c.get(&req).await.is_err());
        assert_eq!(fake.calls.load(Ordering::SeqCst), 2);

        // Far-future expires_at is cached cleanly.
        let mut future = admitted(&[("organization", "01o")], &["*"]);
        future.expires_at = Some("2099-01-01T00:00:00Z".into());
        let (c, fake) = cache(Ok(future));
        assert!(c.get(&req).await.is_ok());
        assert!(c.get(&req).await.is_ok());
        assert_eq!(fake.calls.load(Ordering::SeqCst), 1);
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
        // A service account has no user: its answer is whole.
        let (c, _) = cache(Ok(service_account_user()));
        assert!(c.get(&user_req).await.is_ok());

        // A valid answer with an unparseable CIDR is rejected (503) and not cached.
        let (c, fake) = cache(Ok(AdmitAnswer {
            allowed_cidrs: vec!["not-a-cidr".into()],
            ..admitted(&[("organization", "01o"), ("project", "01p")], &[])
        }));
        assert!(c.get(&req).await.is_err());
        assert!(c.get(&req).await.is_err());
        assert_eq!(fake.calls.load(Ordering::SeqCst), 2);
    }

    #[test]
    fn allows_ip_matches_cidrs_and_fails_closed() {
        let mut ans = admitted(&[("organization", "01o")], &[]);
        assert!(ans.allows_ip("203.0.113.7"));
        assert!(ans.allows_ip("not-an-ip"));

        ans.allowed_cidrs = vec!["203.0.113.0/24".into(), "2001:db8::1/128".into()];
        assert!(ans.allows_ip("203.0.113.7"));
        assert!(ans.allows_ip("::ffff:203.0.113.99"));
        assert!(ans.allows_ip("2001:db8::1"));
        assert!(!ans.allows_ip("198.51.100.1"));
        assert!(!ans.allows_ip("2001:db8::2"));
        assert!(!ans.allows_ip("not-an-ip"));
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

    #[tokio::test]
    async fn invalidation_forces_re_admit_and_handles_in_flight_race_and_lagging_replica() {
        use crate::auth::invalidations::{FeedPage, InvalidationEvent};

        let req = request(key("k"), &[("organization", "acme")], false);
        let initial_ans = AdmitAnswer {
            invalidation_seq: 10,
            config_revision: 5,
            ..admitted(&[("organization", "01o")], &["project:write"])
        };
        let fake = Arc::new(FakeAdmit::new(Ok(initial_ans)));
        let tracker = InvalidationTracker::new(Duration::from_secs(300));
        tracker
            .apply_page(
                FeedPage {
                    head: 10,
                    config_revision: 5,
                    reset: false,
                    events: vec![],
                },
                true,
            )
            .await;
        let c = Arc::new(AdmitCache::with_tracker(
            fake.clone(),
            Duration::from_secs(300),
            Duration::from_secs(10),
            tracker.clone(),
        ));

        // 1. Cached hit stays at 1 call.
        assert_eq!(
            c.get(&req).await.unwrap().permissions,
            vec!["project:write"]
        );
        assert_eq!(
            c.get(&req).await.unwrap().permissions,
            vec!["project:write"]
        );
        assert_eq!(fake.calls.load(Ordering::SeqCst), 1);

        // 2. Member invalidation at seq 11 forces immediate re-admit.
        *fake.answer.lock().unwrap() = Ok(AdmitAnswer {
            invalidation_seq: 11,
            config_revision: 5,
            ..admitted(&[("organization", "01o")], &["project:read"])
        });
        tracker
            .apply_page(
                FeedPage {
                    head: 11,
                    config_revision: 5,
                    reset: false,
                    events: vec![InvalidationEvent {
                        id: 11,
                        scope: "member".into(),
                        target: MEMBER.into(),
                    }],
                },
                false,
            )
            .await;
        assert_eq!(c.get(&req).await.unwrap().permissions, vec!["project:read"]);
        assert_eq!(fake.calls.load(Ordering::SeqCst), 2);

        // 3. In-flight race: invalidation arrives while slow admit is in flight.
        let slow_fake = Arc::new(FakeAdmit {
            delay: Duration::from_millis(60),
            ..FakeAdmit::new(Ok(AdmitAnswer {
                invalidation_seq: 11,
                config_revision: 5,
                ..admitted(&[("organization", "01o")], &["admin"])
            }))
        });
        let slow_cache = Arc::new(AdmitCache::with_tracker(
            slow_fake.clone(),
            Duration::from_secs(300),
            Duration::from_secs(10),
            tracker.clone(),
        ));
        let task = {
            let slow_cache = slow_cache.clone();
            let req = req.clone();
            tokio::spawn(async move { slow_cache.get(&req).await.unwrap() })
        };
        tokio::time::sleep(Duration::from_millis(15)).await;
        tracker
            .apply_page(
                FeedPage {
                    head: 12,
                    config_revision: 5,
                    reset: false,
                    events: vec![InvalidationEvent {
                        id: 12,
                        scope: "member".into(),
                        target: MEMBER.into(),
                    }],
                },
                false,
            )
            .await;
        let _ = task.await.unwrap();
        // Next request MUST re-admit because entry was stamped with seq 11 < 12.
        *slow_fake.answer.lock().unwrap() = Ok(AdmitAnswer {
            invalidation_seq: 12,
            config_revision: 5,
            ..admitted(&[("organization", "01o")], &[])
        });
        assert!(slow_cache.get(&req).await.unwrap().permissions.is_empty());
        assert_eq!(slow_fake.calls.load(Ordering::SeqCst), 2);

        // 4. Lagging identity replica (config_revision 5 < known 6) is returned uncached.
        tracker
            .apply_page(
                FeedPage {
                    head: 12,
                    config_revision: 6,
                    reset: false,
                    events: vec![],
                },
                false,
            )
            .await;
        *fake.answer.lock().unwrap() = Ok(AdmitAnswer {
            invalidation_seq: 12,
            config_revision: 5,
            ..admitted(&[("organization", "01o")], &["stale"])
        });
        assert_eq!(c.get(&req).await.unwrap().permissions, vec!["stale"]);
        // Not cached, so a second call hits identity again.
        assert_eq!(c.get(&req).await.unwrap().permissions, vec!["stale"]);
        assert_eq!(fake.calls.load(Ordering::SeqCst), 4);
    }
}
