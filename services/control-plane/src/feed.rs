//! The control plane's revision, as readers long-poll it. Each process polls
//! the head in Postgres, so a write through any replica reaches readers of
//! every replica. A write through this process advances it at once.

use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::time::Duration;

use tokio::sync::watch;
use tracing::warn;

use crate::store::Store;

const POLL_INTERVAL: Duration = Duration::from_secs(1);

#[derive(Clone)]
pub struct Feed {
    head: watch::Sender<i64>,
    write_epoch: Arc<AtomicU64>,
    shutdown: watch::Receiver<bool>,
}

impl Feed {
    /// Starts polling the head. `initial` is the head read at boot.
    pub fn spawn(pg: Store, initial: i64, shutdown: watch::Receiver<bool>) -> Self {
        let (head, _) = watch::channel(initial);
        let feed = Self {
            head,
            write_epoch: Arc::new(AtomicU64::new(0)),
            shutdown,
        };
        let poller = feed.clone();
        let mut poller_shutdown = feed.shutdown.clone();
        tokio::spawn(async move {
            let mut interval = tokio::time::interval(POLL_INTERVAL);
            interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
            loop {
                tokio::select! {
                    _ = wait_shutdown(&mut poller_shutdown) => break,
                    _ = interval.tick() => {}
                }
                let epoch = poller.write_epoch();
                match pg.head().await {
                    Ok(revision) => poller.sync(revision, epoch),
                    Err(e) => warn!(error = %e, "control-plane head poll failed"),
                }
            }
        });
        feed
    }

    #[cfg(test)]
    pub(crate) fn new_for_test(initial: i64, shutdown: watch::Receiver<bool>) -> Self {
        let (head, _) = watch::channel(initial);
        Self {
            head,
            write_epoch: Arc::new(AtomicU64::new(0)),
            shutdown,
        }
    }

    fn write_epoch(&self) -> u64 {
        self.write_epoch.load(Ordering::Acquire)
    }

    /// Moves the head forward after a local write. Never backward.
    pub fn advance(&self, revision: i64) {
        self.head.send_if_modified(|current| {
            self.write_epoch.fetch_add(1, Ordering::AcqRel);
            if revision > *current {
                *current = revision;
                true
            } else {
                false
            }
        });
    }

    /// Reconciles the head with a Postgres poll read, including downward on a
    /// store reset, unless a local write occurred since `observed_epoch`.
    fn sync(&self, revision: i64, observed_epoch: u64) {
        self.head.send_if_modified(|current| {
            if self.write_epoch.load(Ordering::Acquire) != observed_epoch {
                return false;
            }
            if revision != *current {
                *current = revision;
                true
            } else {
                false
            }
        });
    }

    /// Waits until the head differs from `after` (advanced or reset), for at
    /// most `wait`. Returns `false` on timeout or graceful shutdown.
    pub async fn wait_past(&self, after: i64, wait: Duration) -> bool {
        let mut rx = self.head.subscribe();
        let mut shutdown = self.shutdown.clone();
        if *shutdown.borrow_and_update() {
            return false;
        }
        tokio::select! {
            _ = wait_shutdown(&mut shutdown) => false,
            res = tokio::time::timeout(wait, rx.wait_for(|head| *head != after)) => {
                res.is_ok_and(|r| r.is_ok())
            }
        }
    }
}

pub(crate) async fn wait_shutdown(rx: &mut watch::Receiver<bool>) {
    loop {
        if *rx.borrow_and_update() {
            return;
        }
        if rx.changed().await.is_err() {
            return;
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn feed_at(revision: i64) -> (Feed, watch::Sender<bool>) {
        let (shutdown_tx, shutdown_rx) = watch::channel(false);
        (Feed::new_for_test(revision, shutdown_rx), shutdown_tx)
    }

    #[tokio::test]
    async fn past_returns_at_once() {
        let (feed, _tx) = feed_at(5);
        assert!(feed.wait_past(4, Duration::from_millis(10)).await);
    }

    #[tokio::test]
    async fn waits_for_an_advance() {
        let (feed, _tx) = feed_at(5);
        let writer = feed.clone();
        let wait = tokio::spawn(async move { feed.wait_past(5, Duration::from_secs(5)).await });
        tokio::time::sleep(Duration::from_millis(20)).await;
        writer.advance(6);
        assert!(wait.await.unwrap());
    }

    #[tokio::test]
    async fn times_out_and_never_goes_back() {
        let (feed, _tx) = feed_at(5);
        feed.advance(3);
        assert!(!feed.wait_past(5, Duration::from_millis(20)).await);
        assert!(feed.wait_past(4, Duration::from_millis(10)).await);
    }

    #[tokio::test]
    async fn downward_sync_on_reset_wakes_waiters() {
        let (feed, _tx) = feed_at(5);
        let poller = feed.clone();
        let wait = tokio::spawn(async move { feed.wait_past(5, Duration::from_secs(5)).await });
        tokio::time::sleep(Duration::from_millis(20)).await;
        let epoch = poller.write_epoch();
        poller.sync(0, epoch);
        assert!(wait.await.unwrap());
        assert_eq!(*poller.head.borrow(), 0);
    }

    #[tokio::test]
    async fn stale_poll_does_not_overwrite_local_advance() {
        let (feed, _tx) = feed_at(5);
        let epoch_before = feed.write_epoch();
        feed.advance(6);
        feed.sync(5, epoch_before);
        assert_eq!(*feed.head.borrow(), 6);
    }

    #[tokio::test]
    async fn shutdown_wakes_waiter_immediately() {
        let (feed, shutdown_tx) = feed_at(5);
        let wait = tokio::spawn(async move { feed.wait_past(5, Duration::from_secs(25)).await });
        tokio::time::sleep(Duration::from_millis(20)).await;
        shutdown_tx.send(true).unwrap();
        let reached = tokio::time::timeout(Duration::from_secs(1), wait)
            .await
            .expect("wait_past should wake promptly on shutdown")
            .unwrap();
        assert!(!reached);
    }
}
