//! The control plane's revision, as readers long-poll it. Each process polls
//! the head in Postgres, so a write through any replica reaches readers of
//! every replica. A write through this process advances it at once.

use std::time::Duration;

use tokio::sync::watch;
use tracing::warn;

use crate::pg_store::PgStore;

const POLL_INTERVAL: Duration = Duration::from_secs(1);

#[derive(Clone)]
pub struct Feed {
    head: watch::Sender<i64>,
}

impl Feed {
    /// Starts polling the head. `initial` is the head read at boot.
    pub fn spawn(pg: PgStore, initial: i64) -> Self {
        let (head, _) = watch::channel(initial);
        let feed = Self { head };
        let poller = feed.clone();
        tokio::spawn(async move {
            let mut interval = tokio::time::interval(POLL_INTERVAL);
            interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
            loop {
                interval.tick().await;
                match pg.head().await {
                    Ok(revision) => poller.advance(revision),
                    Err(e) => warn!(error = %e, "control-plane head poll failed"),
                }
            }
        });
        feed
    }

    /// The latest head this process has seen.
    pub fn current(&self) -> i64 {
        *self.head.borrow()
    }

    /// Moves the head forward. Never backward.
    pub fn advance(&self, revision: i64) {
        self.head.send_if_modified(|current| {
            if revision > *current {
                *current = revision;
                true
            } else {
                false
            }
        });
    }

    /// Waits until the head is past `after`, for at most `wait`. True if it is.
    pub async fn wait_past(&self, after: i64, wait: Duration) -> bool {
        let mut rx = self.head.subscribe();
        let reached = tokio::time::timeout(wait, rx.wait_for(|head| *head > after))
            .await
            .is_ok_and(|r| r.is_ok());
        reached
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn feed_at(revision: i64) -> Feed {
        let (head, _) = watch::channel(revision);
        Feed { head }
    }

    #[tokio::test]
    async fn past_returns_at_once() {
        let feed = feed_at(5);
        assert!(feed.wait_past(4, Duration::from_millis(10)).await);
    }

    #[tokio::test]
    async fn waits_for_an_advance() {
        let feed = feed_at(5);
        let writer = feed.clone();
        let wait = tokio::spawn(async move { feed.wait_past(5, Duration::from_secs(5)).await });
        tokio::time::sleep(Duration::from_millis(20)).await;
        writer.advance(6);
        assert!(wait.await.unwrap());
    }

    #[tokio::test]
    async fn times_out_and_never_goes_back() {
        let feed = feed_at(5);
        feed.advance(3);
        assert!(!feed.wait_past(5, Duration::from_millis(20)).await);
        assert!(feed.wait_past(4, Duration::from_millis(10)).await);
    }
}
