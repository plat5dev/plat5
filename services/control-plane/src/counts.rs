//! Identity's counts for a plan: the nodes of each kind the plan removes or
//! moves under a new parent (docs/reference/api/internal.md#plan-counts). Asked
//! only when the plan does one of those.

use std::collections::BTreeMap;
use std::future::Future;
use std::pin::Pin;
use std::time::Duration;

use serde::{Deserialize, Serialize};

#[derive(Clone, Debug, Default, PartialEq, Serialize)]
pub struct CountsRequest {
    pub kinds: Vec<String>,
}

#[derive(Clone, Debug, Default, PartialEq, Deserialize, Serialize)]
pub struct CountsResponse {
    pub kinds: BTreeMap<String, KindCounts>,
}

/// One kind's `nodes`, live and deleted, and up to five of their ids, live ones
/// first.
#[derive(Clone, Debug, Default, PartialEq, Deserialize, Serialize)]
pub struct KindCounts {
    pub nodes: u64,
    pub sample: Vec<String>,
}

pub type CountsFuture<'a> =
    Pin<Box<dyn Future<Output = Result<CountsResponse, String>> + Send + 'a>>;

pub trait Counts: Send + Sync {
    fn plan_counts(&self, request: CountsRequest) -> CountsFuture<'_>;
}

/// `POST {IDENTITY_INTERNAL_URL}/internal/plan`.
pub struct HttpCounts {
    url: String,
    http: reqwest::Client,
}

impl HttpCounts {
    pub fn new(base_url: &str) -> Self {
        Self {
            url: format!("{}/internal/plan", base_url.trim_end_matches('/')),
            http: reqwest::Client::builder()
                .timeout(Duration::from_secs(10))
                .build()
                .expect("http client"),
        }
    }
}

impl Counts for HttpCounts {
    fn plan_counts(&self, request: CountsRequest) -> CountsFuture<'_> {
        Box::pin(async move {
            let resp = self
                .http
                .post(&self.url)
                .json(&request)
                .send()
                .await
                .map_err(|e| format!("identity: {e}"))?;
            let status = resp.status();
            if !status.is_success() {
                return Err(format!("identity: HTTP {status}"));
            }
            let counts: CountsResponse = resp
                .json()
                .await
                .map_err(|e| format!("identity: bad plan counts: {e}"))?;
            if let Some(kind) = request
                .kinds
                .iter()
                .find(|k| !counts.kinds.contains_key(*k))
            {
                return Err(format!("identity: no count for kind {kind}"));
            }
            Ok(counts)
        })
    }
}

/// Counts from a table, for tests: what identity would answer.
#[cfg(test)]
pub struct FixedCounts(
    pub std::sync::Mutex<CountsResponse>,
    pub std::sync::atomic::AtomicUsize,
);

#[cfg(test)]
impl FixedCounts {
    pub fn new(counts: CountsResponse) -> Self {
        Self(
            std::sync::Mutex::new(counts),
            std::sync::atomic::AtomicUsize::new(0),
        )
    }

    pub fn set(&self, counts: CountsResponse) {
        *self.0.lock().unwrap() = counts;
    }

    pub fn calls(&self) -> usize {
        self.1.load(std::sync::atomic::Ordering::SeqCst)
    }
}

#[cfg(test)]
impl Counts for FixedCounts {
    fn plan_counts(&self, request: CountsRequest) -> CountsFuture<'_> {
        self.1.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
        let all = self.0.lock().unwrap().clone();
        Box::pin(async move {
            Ok(CountsResponse {
                kinds: request
                    .kinds
                    .iter()
                    .map(|k| (k.clone(), all.kinds.get(k).cloned().unwrap_or_default()))
                    .collect(),
            })
        })
    }
}

/// Identity is down.
#[cfg(test)]
pub struct NoCounts;

#[cfg(test)]
impl Counts for NoCounts {
    fn plan_counts(&self, _: CountsRequest) -> CountsFuture<'_> {
        Box::pin(async { Err("identity: connection refused".to_string()) })
    }
}
