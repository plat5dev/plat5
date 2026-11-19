//! Identity's counts for a plan: the nodes of each kind the plan removes or
//! moves under a new parent (docs/reference/api/internal.md#plan-counts). Asked
//! only when the plan does one of those.

use std::collections::{BTreeMap, HashMap};
use std::future::Future;
use std::pin::Pin;
use std::time::Duration;

use opentelemetry::global;
use serde::{Deserialize, Serialize};
use tracing_opentelemetry::OpenTelemetrySpanExt;

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
    fn plan_counts(&self, request: CountsRequest, request_id: &str) -> CountsFuture<'_>;
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
    fn plan_counts(&self, request: CountsRequest, request_id: &str) -> CountsFuture<'_> {
        let request_id = request_id.to_string();
        Box::pin(async move {
            let mut req = self.http.post(&self.url).json(&request);
            if !request_id.is_empty() {
                req = req.header("x-request-id", &request_id);
            }
            let mut trace_headers = HashMap::new();
            let cx = tracing::Span::current().context();
            global::get_text_map_propagator(|p| p.inject_context(&cx, &mut trace_headers));
            for (k, v) in trace_headers {
                req = req.header(k, v);
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
    fn plan_counts(&self, request: CountsRequest, _request_id: &str) -> CountsFuture<'_> {
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
    fn plan_counts(&self, _: CountsRequest, _request_id: &str) -> CountsFuture<'_> {
        Box::pin(async { Err("identity: connection refused".to_string()) })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::http::HeaderMap;
    use axum::routing::post;
    use axum::{Json, Router};
    use opentelemetry::trace::TracerProvider;
    use opentelemetry_sdk::propagation::TraceContextPropagator;
    use opentelemetry_sdk::trace as sdktrace;
    use std::sync::{Arc, Mutex};
    use tracing::Instrument;
    use tracing_opentelemetry::OpenTelemetryLayer;
    use tracing_subscriber::layer::SubscriberExt;

    #[tokio::test]
    async fn propagates_traceparent_and_request_id() {
        global::set_text_map_propagator(TraceContextPropagator::new());
        let provider = sdktrace::SdkTracerProvider::builder().build();
        let subscriber =
            tracing_subscriber::registry().with(OpenTelemetryLayer::new(provider.tracer("test")));
        let _guard = tracing::subscriber::set_default(subscriber);

        let seen: Arc<Mutex<HashMap<String, String>>> = Arc::new(Mutex::new(HashMap::new()));
        let seen_handler = seen.clone();
        let app = Router::new().route(
            "/internal/plan",
            post(move |headers: HeaderMap| {
                let seen = seen_handler.clone();
                async move {
                    let mut map = seen.lock().unwrap();
                    for (k, v) in &headers {
                        if let Ok(s) = v.to_str() {
                            map.insert(k.as_str().to_string(), s.to_string());
                        }
                    }
                    Json(CountsResponse {
                        kinds: [("folder".to_string(), KindCounts::default())].into(),
                    })
                }
            }),
        );

        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let server = tokio::spawn(async move {
            axum::serve(listener, app).await.unwrap();
        });

        let trace_id = "4bf92f3577b34da6a3ce929d0e0e4736";
        let incoming = HashMap::from([(
            "traceparent".to_string(),
            format!("00-{trace_id}-00f067aa0ba902b7-01"),
        )]);
        let parent_cx = global::get_text_map_propagator(|p| p.extract(&incoming));

        let span = tracing::info_span!("http_test");
        let _ = span.set_parent(parent_cx);

        let counts = HttpCounts::new(&format!("http://{addr}"));
        let resp = counts
            .plan_counts(
                CountsRequest {
                    kinds: vec!["folder".to_string()],
                },
                "req-abc-123",
            )
            .instrument(span)
            .await
            .unwrap();
        assert_eq!(resp.kinds["folder"].nodes, 0);

        server.abort();

        let headers = seen.lock().unwrap();
        assert_eq!(
            headers.get("x-request-id").map(String::as_str),
            Some("req-abc-123")
        );
        let tp = headers
            .get("traceparent")
            .expect("traceparent header must be sent");
        assert!(
            tp.starts_with(&format!("00-{trace_id}-")),
            "expected trace_id {trace_id} in traceparent {tp}"
        );
    }
}
