//! Identity's counts for a plan: what removing a kind would delete, and what's in
//! the way of narrowing a parent (docs/reference/api/internal.md#plan-counts).
//! Asked only when the plan removes a kind or narrows a parent.

use std::collections::BTreeMap;
use std::future::Future;
use std::pin::Pin;
use std::time::Duration;

use serde::{Deserialize, Serialize};

#[derive(Clone, Debug, Default, PartialEq, Serialize)]
pub struct CountsRequest {
    pub kinds_removed: Vec<String>,
    pub parents_narrowed: Vec<Narrowed>,
}

#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct Narrowed {
    pub kind: String,
    pub removed: Vec<String>,
}

#[derive(Clone, Debug, Default, PartialEq, Deserialize, Serialize)]
pub struct CountsResponse {
    pub kinds_removed: BTreeMap<String, Effects>,
    pub parents_narrowed: BTreeMap<String, InTheWay>,
}

/// What removing a kind deletes: its nodes and descendants, live and deleted,
/// and what hangs off them.
#[derive(Clone, Debug, Default, PartialEq, Deserialize, Serialize)]
pub struct Effects {
    pub nodes: u64,
    pub bindings: u64,
    pub service_accounts: u64,
    pub keys: u64,
}

/// Nodes under a parent kind the config no longer allows, with a few ids.
#[derive(Clone, Debug, Default, PartialEq, Deserialize, Serialize)]
pub struct InTheWay {
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
    internal_token: Option<String>,
    http: reqwest::Client,
}

impl HttpCounts {
    pub fn new(base_url: &str, internal_token: Option<String>) -> Self {
        Self {
            url: format!("{}/internal/plan", base_url.trim_end_matches('/')),
            internal_token,
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
            let mut req = self.http.post(&self.url).json(&request);
            if let Some(token) = &self.internal_token {
                req = req.header("x-plat5-internal-token", token);
            }
            let resp = req.send().await.map_err(|e| format!("identity: {e}"))?;
            let status = resp.status();
            if !status.is_success() {
                return Err(format!("identity: HTTP {status}"));
            }
            let counts: CountsResponse = resp
                .json()
                .await
                .map_err(|e| format!("identity: bad plan counts: {e}"))?;
            for kind in &request.kinds_removed {
                if !counts.kinds_removed.contains_key(kind) {
                    return Err(format!("identity: no count for removed kind {kind}"));
                }
            }
            for n in &request.parents_narrowed {
                if !counts.parents_narrowed.contains_key(&n.kind) {
                    return Err(format!("identity: no count for narrowed kind {}", n.kind));
                }
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
                kinds_removed: request
                    .kinds_removed
                    .iter()
                    .map(|k| {
                        (
                            k.clone(),
                            all.kinds_removed.get(k).cloned().unwrap_or_default(),
                        )
                    })
                    .collect(),
                parents_narrowed: request
                    .parents_narrowed
                    .iter()
                    .map(|n| {
                        (
                            n.kind.clone(),
                            all.parents_narrowed
                                .get(&n.kind)
                                .cloned()
                                .unwrap_or_default(),
                        )
                    })
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
