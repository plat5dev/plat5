//! Long-poll follower and epoch table for identity's admit invalidation feed
//! (`GET /internal/admit-invalidations`, docs/reference/api/internal.md#admit-invalidations).
//!
//! Instead of reverse-indexing `AdmitCache`'s blake3 keys, each cached positive
//! admit entry records `(global_gen, seq, config_revision)` and is checked in
//! O(1) against TTL-bounded epoch maps (`key`, `user`, `member`, `root`) plus
//! `global_gen` (for resets / DB wipes) and `min_config_revision` (for role or
//! kind applies).

use std::sync::atomic::{AtomicBool, AtomicI64, AtomicU64, Ordering};
use std::sync::Arc;
use std::time::Duration;

use moka::future::Cache;
use moka::ops::compute::Op;
use serde::Deserialize;
use tracing::{info, warn};

use crate::auth::admit::{AdmitAnswer, AdmitCredential, AdmitRequest};
use crate::metrics;

const EPOCH_CACHE_CAPACITY: u64 = 100_000;
const POLL_TIMEOUT: Duration = Duration::from_secs(40);
const RETRY_BACKOFF: &[Duration] = &[
    Duration::from_secs(1),
    Duration::from_secs(2),
    Duration::from_secs(5),
    Duration::from_secs(10),
];

#[derive(Debug, Clone, Deserialize, PartialEq, Eq)]
pub struct InvalidationEvent {
    pub id: i64,
    pub scope: String,
    pub target: String,
}

#[derive(Debug, Clone, Deserialize, PartialEq, Eq)]
pub struct FeedPage {
    pub head: i64,
    pub config_revision: i64,
    pub reset: bool,
    pub events: Vec<InvalidationEvent>,
}

/// Shared invalidation state consulted by [`crate::auth::admit::AdmitCache`]
/// on every lookup and updated by the background feed follower.
#[derive(Clone)]
pub struct InvalidationTracker {
    inner: Arc<Inner>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
enum EpochScope {
    Key,
    User,
    Member,
    Root,
}

impl EpochScope {
    fn from_wire(s: &str) -> Option<Self> {
        match s {
            "key" => Some(Self::Key),
            "user" => Some(Self::User),
            "member" => Some(Self::Member),
            "root" => Some(Self::Root),
            _ => None,
        }
    }

    fn as_str(self) -> &'static str {
        match self {
            Self::Key => "key",
            Self::User => "user",
            Self::Member => "member",
            Self::Root => "root",
        }
    }
}

struct Inner {
    global_gen: AtomicU64,
    cursor: AtomicI64,
    user_seq: AtomicI64,
    synced: AtomicBool,
    min_config_revision: AtomicI64,
    epochs: Cache<(EpochScope, String), i64>,
}

/// Snapshot taken right before calling `/internal/admit` so an invalidation
/// arriving while admit is in flight can never be hidden behind a newer cursor.
#[derive(Clone, Copy, Debug)]
pub struct AdmitStamp {
    pub global_gen: u64,
    pub cursor: i64,
    pub synced: bool,
}

impl InvalidationTracker {
    pub fn new(ttl: Duration) -> Self {
        let cap_ttl = ttl.max(Duration::from_secs(1));
        let epochs = Cache::builder()
            .max_capacity(EPOCH_CACHE_CAPACITY)
            .time_to_live(cap_ttl)
            .build();
        Self {
            inner: Arc::new(Inner {
                global_gen: AtomicU64::new(0),
                cursor: AtomicI64::new(0),
                user_seq: AtomicI64::new(0),
                synced: AtomicBool::new(false),
                min_config_revision: AtomicI64::new(0),
                epochs,
            }),
        }
    }

    pub fn snapshot_before_admit(&self) -> AdmitStamp {
        let synced = self.inner.synced.load(Ordering::Acquire);
        let global_gen = self.inner.global_gen.load(Ordering::Acquire);
        let cursor = self.inner.cursor.load(Ordering::Acquire);
        AdmitStamp {
            global_gen,
            cursor,
            synced,
        }
    }

    /// Monotonically advances `min_config_revision` and returns the current floor.
    pub fn observe_config_revision(&self, rev: i64) -> i64 {
        let prev = self
            .inner
            .min_config_revision
            .fetch_max(rev, Ordering::AcqRel);
        prev.max(rev)
    }

    pub fn min_config_revision(&self) -> i64 {
        self.inner.min_config_revision.load(Ordering::Acquire)
    }

    pub fn cursor(&self) -> i64 {
        self.inner.cursor.load(Ordering::Acquire)
    }

    /// Computes the sequence stamped onto a newly cached admit answer.
    /// Uses `min(stamp.cursor, answer_seq)` once synced so an in-flight
    /// invalidation can never be skipped.
    pub fn entry_seq(&self, stamp: AdmitStamp, answer_seq: i64) -> i64 {
        if stamp.synced {
            stamp.cursor.min(answer_seq)
        } else {
            0
        }
    }

    /// Whether a cached positive entry is still valid under every invalidation epoch.
    pub async fn is_fresh(
        &self,
        answer: &AdmitAnswer,
        global_gen: u64,
        seq: i64,
        config_revision: i64,
    ) -> bool {
        if global_gen != self.inner.global_gen.load(Ordering::Acquire) {
            return false;
        }
        if config_revision < self.inner.min_config_revision.load(Ordering::Acquire) {
            return false;
        }
        let targets = [
            answer.key_id.as_deref().map(|t| (EpochScope::Key, t)),
            answer.user_id.as_deref().map(|t| (EpochScope::User, t)),
            answer.member_id.as_deref().map(|t| (EpochScope::Member, t)),
            answer
                .nodes
                .first()
                .map(|r| (EpochScope::Root, r.id.as_str())),
        ];
        for (scope, target) in targets.into_iter().flatten() {
            if self
                .inner
                .epochs
                .get(&(scope, target.to_string()))
                .await
                .is_some_and(|e| e > seq)
            {
                return false;
            }
        }
        true
    }

    /// Whether a cached negative entry is still valid.
    ///
    /// - Valid credentials refused on a tree route (`valid: true`, `member_id: None`,
    ///   e.g. outside the tree, suspended member, or org security policy) are invalidated
    ///   when any commit-ordered invalidation event or config revision arrives past the
    ///   entry's stamp, so invite redeem, member reactivation, and policy updates take
    ///   effect without waiting for `ADMIT_NEGATIVE_CACHE_TTL`.
    /// - Invalid JWTs (`valid: false`, e.g. a platform-suspended person) are invalidated
    ///   on `user` events or DB resets.
    /// - Invalid API keys (`valid: false`, e.g. unknown or revoked keys) are only
    ///   invalidated on DB resets (`global_gen`), preserving negative-cache protection
    ///   against bad-key floods.
    pub fn is_negative_fresh(
        &self,
        answer: &AdmitAnswer,
        req: &AdmitRequest,
        global_gen: u64,
        seq: i64,
        config_revision: i64,
    ) -> bool {
        if global_gen != self.inner.global_gen.load(Ordering::Acquire) {
            return false;
        }
        if answer.valid {
            if config_revision < self.inner.min_config_revision.load(Ordering::Acquire) {
                return false;
            }
            return self.inner.cursor.load(Ordering::Acquire) <= seq;
        }
        if matches!(req.credential, AdmitCredential::Idp { .. }) {
            return self.inner.user_seq.load(Ordering::Acquire) <= seq;
        }
        true
    }

    /// Applies one page from `GET /internal/admit-invalidations`.
    pub async fn apply_page(&self, page: FeedPage, initial_sync: bool) {
        let prev_rev = self
            .inner
            .min_config_revision
            .fetch_max(page.config_revision, Ordering::AcqRel);
        if !initial_sync && page.config_revision > prev_rev {
            metrics::record_admit_invalidation("config_revision");
        }

        if initial_sync {
            self.inner.cursor.store(page.head, Ordering::Release);
            self.inner.user_seq.store(page.head, Ordering::Release);
            self.inner.synced.store(true, Ordering::Release);
            // Flush anything cached before initial sync connected.
            self.inner.global_gen.fetch_add(1, Ordering::AcqRel);
            return;
        }

        if page.reset {
            self.inner.global_gen.fetch_add(1, Ordering::AcqRel);
            self.inner.cursor.store(page.head, Ordering::Release);
            self.inner.user_seq.store(page.head, Ordering::Release);
            metrics::record_admit_invalidation("reset");
            return;
        }

        let mut max_seen = self.inner.cursor.load(Ordering::Acquire);
        for ev in page.events {
            if ev.id > max_seen {
                max_seen = ev.id;
            }
            match EpochScope::from_wire(&ev.scope) {
                Some(scope) => {
                    if scope == EpochScope::User {
                        self.inner.user_seq.fetch_max(ev.id, Ordering::AcqRel);
                    }
                    bump_epoch(&self.inner.epochs, (scope, ev.target), ev.id).await;
                    metrics::record_admit_invalidation(scope.as_str());
                }
                None => {
                    warn!(
                        scope = %ev.scope,
                        id = ev.id,
                        "ignoring unknown admit invalidation scope"
                    );
                }
            }
        }
        self.inner.cursor.store(max_seen, Ordering::Release);
    }

    /// Spawns the background long-poll follower against identity's internal port.
    pub fn spawn_follower(&self, identity_internal_url: String, runtime: &tokio::runtime::Handle) {
        let tracker = self.clone();
        let base_url = format!(
            "{}/internal/admit-invalidations",
            identity_internal_url.trim_end_matches('/')
        );
        let client = reqwest::Client::builder()
            .timeout(POLL_TIMEOUT)
            .build()
            .expect("admit invalidation http client");
        runtime.spawn(async move {
            follow_loop(tracker, client, base_url).await;
        });
    }
}

async fn bump_epoch(cache: &Cache<(EpochScope, String), i64>, key: (EpochScope, String), seq: i64) {
    cache
        .entry(key)
        .and_compute_with(|existing| async move {
            match existing {
                Some(entry) if *entry.value() >= seq => Op::Nop,
                _ => Op::Put(seq),
            }
        })
        .await;
}

async fn follow_loop(tracker: InvalidationTracker, client: reqwest::Client, base_url: String) {
    let mut attempt = 0usize;
    let mut disconnected_logged = false;

    // 1. Initial sync (no `after`) to learn current head & config_revision.
    loop {
        match fetch_page(&client, &base_url).await {
            Ok(Some(page)) => {
                let head = page.head;
                let rev = page.config_revision;
                tracker.apply_page(page, true).await;
                info!(
                    head,
                    config_revision = rev,
                    "admit invalidation feed synced"
                );
                attempt = 0;
                disconnected_logged = false;
                break;
            }
            Ok(None) => {}
            Err(err) => {
                if !disconnected_logged {
                    warn!(error = %err, "admit invalidation feed unavailable at startup; using TTL fallback");
                    disconnected_logged = true;
                }
            }
        }
        let delay = RETRY_BACKOFF[attempt.min(RETRY_BACKOFF.len() - 1)];
        attempt = attempt.saturating_add(1);
        tokio::time::sleep(delay).await;
    }

    // 2. Steady-state long-poll loop.
    loop {
        let after = tracker.cursor();
        let rev = tracker.min_config_revision();
        let url = format!("{base_url}?after={after}&config_revision={rev}");
        match fetch_page(&client, &url).await {
            Ok(Some(page)) => {
                if disconnected_logged {
                    info!(
                        head = page.head,
                        config_revision = page.config_revision,
                        "admit invalidation feed reconnected"
                    );
                    disconnected_logged = false;
                }
                attempt = 0;
                tracker.apply_page(page, false).await;
            }
            Ok(None) => {
                if disconnected_logged {
                    info!("admit invalidation feed reconnected");
                    disconnected_logged = false;
                }
                attempt = 0;
            }
            Err(err) => {
                if !disconnected_logged {
                    warn!(error = %err, "admit invalidation feed disconnected; falling back to TTL until reconnected");
                    disconnected_logged = true;
                }
                let delay = RETRY_BACKOFF[attempt.min(RETRY_BACKOFF.len() - 1)];
                attempt = attempt.saturating_add(1);
                tokio::time::sleep(delay).await;
            }
        }
    }
}

async fn fetch_page(client: &reqwest::Client, url: &str) -> Result<Option<FeedPage>, String> {
    let resp = client.get(url).send().await.map_err(|e| e.to_string())?;
    let status = resp.status();
    if status == reqwest::StatusCode::NO_CONTENT {
        return Ok(None);
    }
    if !status.is_success() {
        return Err(format!("status {status}"));
    }
    let page = resp
        .json::<FeedPage>()
        .await
        .map_err(|e| format!("invalid json: {e}"))?;
    Ok(Some(page))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::auth::admit::tests::{admitted, user, KEY_ID, MEMBER, USER};

    #[tokio::test]
    async fn targeted_epochs_invalidate_matching_answers_only() {
        let tracker = InvalidationTracker::new(Duration::from_secs(300));
        tracker
            .apply_page(
                FeedPage {
                    head: 10,
                    config_revision: 1,
                    reset: false,
                    events: vec![],
                },
                true,
            )
            .await;

        let stamp = tracker.snapshot_before_admit();
        let tree_ans = admitted(
            &[("organization", "01org1"), ("project", "01proj1")],
            &["project:read"],
        );
        let other_tree = admitted(&[("organization", "01org2")], &["organization:read"]);
        let user_ans = user();

        let seq = tracker.entry_seq(stamp, 10);
        assert!(tracker.is_fresh(&tree_ans, stamp.global_gen, seq, 1).await);
        assert!(
            tracker
                .is_fresh(&other_tree, stamp.global_gen, seq, 1)
                .await
        );
        assert!(tracker.is_fresh(&user_ans, stamp.global_gen, seq, 1).await);

        // Root invalidation on 01org1 only invalidates 01org1.
        tracker
            .apply_page(
                FeedPage {
                    head: 11,
                    config_revision: 1,
                    reset: false,
                    events: vec![InvalidationEvent {
                        id: 11,
                        scope: "root".into(),
                        target: "01org1".into(),
                    }],
                },
                false,
            )
            .await;
        assert!(!tracker.is_fresh(&tree_ans, stamp.global_gen, seq, 1).await);
        assert!(
            tracker
                .is_fresh(&other_tree, stamp.global_gen, seq, 1)
                .await
        );
        assert!(tracker.is_fresh(&user_ans, stamp.global_gen, seq, 1).await);

        // Key invalidation hits answers with KEY_ID, not user_ans (JWT, key_id = None).
        tracker
            .apply_page(
                FeedPage {
                    head: 12,
                    config_revision: 1,
                    reset: false,
                    events: vec![InvalidationEvent {
                        id: 12,
                        scope: "key".into(),
                        target: KEY_ID.into(),
                    }],
                },
                false,
            )
            .await;
        assert!(
            !tracker
                .is_fresh(&other_tree, stamp.global_gen, seq, 1)
                .await
        );
        assert!(tracker.is_fresh(&user_ans, stamp.global_gen, seq, 1).await);

        // User invalidation hits user_ans.
        tracker
            .apply_page(
                FeedPage {
                    head: 13,
                    config_revision: 1,
                    reset: false,
                    events: vec![InvalidationEvent {
                        id: 13,
                        scope: "user".into(),
                        target: USER.into(),
                    }],
                },
                false,
            )
            .await;
        assert!(!tracker.is_fresh(&user_ans, stamp.global_gen, seq, 1).await);

        // Member invalidation hits MEMBER at seq 13.
        let stamp2 = tracker.snapshot_before_admit();
        let seq2 = tracker.entry_seq(stamp2, 13);
        assert!(
            tracker
                .is_fresh(&tree_ans, stamp2.global_gen, seq2, 1)
                .await
        );
        tracker
            .apply_page(
                FeedPage {
                    head: 14,
                    config_revision: 1,
                    reset: false,
                    events: vec![InvalidationEvent {
                        id: 14,
                        scope: "member".into(),
                        target: MEMBER.into(),
                    }],
                },
                false,
            )
            .await;
        assert!(
            !tracker
                .is_fresh(&tree_ans, stamp2.global_gen, seq2, 1)
                .await
        );
    }

    #[tokio::test]
    async fn reset_and_config_revision_invalidate_all_entries() {
        let tracker = InvalidationTracker::new(Duration::from_secs(300));
        tracker
            .apply_page(
                FeedPage {
                    head: 50,
                    config_revision: 3,
                    reset: false,
                    events: vec![],
                },
                true,
            )
            .await;

        let stamp = tracker.snapshot_before_admit();
        let ans = user();
        let seq = tracker.entry_seq(stamp, 50);
        assert!(tracker.is_fresh(&ans, stamp.global_gen, seq, 3).await);

        // Config revision bump invalidates cached rev 3 entries.
        tracker
            .apply_page(
                FeedPage {
                    head: 50,
                    config_revision: 4,
                    reset: false,
                    events: vec![],
                },
                false,
            )
            .await;
        assert!(!tracker.is_fresh(&ans, stamp.global_gen, seq, 3).await);
        assert!(tracker.is_fresh(&ans, stamp.global_gen, seq, 4).await);

        // Reset (e.g. DB wipe where head goes back to 0) invalidates via global_gen.
        tracker
            .apply_page(
                FeedPage {
                    head: 0,
                    config_revision: 4,
                    reset: true,
                    events: vec![],
                },
                false,
            )
            .await;
        assert!(!tracker.is_fresh(&ans, stamp.global_gen, seq, 4).await);
        assert_eq!(tracker.cursor(), 0);
    }
}
