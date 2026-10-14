//! What the gateway runs on: routes and CORS origins from the control plane.
//!
//! At boot the gateway waits for its first snapshot. It then long-polls
//! `GET {CONTROL_PLANE_URL}/internal/gateway?after={revision}` and swaps the
//! whole snapshot on every change. While the control plane is down it keeps
//! serving the last snapshot. Contract: docs/control-plane.md.

use std::collections::HashMap;
use std::sync::Arc;
use std::time::Duration;

use arc_swap::ArcSwap;
use serde::Deserialize;
use tokio::time::sleep;
use tracing::{error, info, warn};

use crate::gateway::CorsPolicy;
use crate::route_config::{Config, ServiceConfig};
use crate::route_map::RouteMap;

const INTERNAL_TOKEN_HEADER: &str = "X-Plat5-Internal-Token";
/// Longer than the control plane's long-poll wait, so an idle poll ends with 204.
const POLL_TIMEOUT: Duration = Duration::from_secs(40);
const MAX_RETRY_DELAY: Duration = Duration::from_secs(10);

/// One control-plane revision, swapped whole.
pub struct Snapshot {
    pub revision: i64,
    pub routes: RouteMap,
    pub cors: Arc<CorsPolicy>,
}

#[derive(Deserialize)]
struct GatewayFeed {
    revision: i64,
    services: HashMap<String, ServiceConfig>,
    cors: CorsFeed,
}

#[derive(Deserialize)]
struct CorsFeed {
    allowed_origins: Vec<String>,
}

#[derive(Debug)]
pub enum ControlPlaneError {
    Network(String),
    Status(u16),
    Decode(String),
    InvalidConfig(String),
}

impl std::fmt::Display for ControlPlaneError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            ControlPlaneError::Network(m) => write!(f, "control plane unreachable: {m}"),
            ControlPlaneError::Status(s) => write!(f, "control plane answered {s}"),
            ControlPlaneError::Decode(m) => write!(f, "control plane answer did not parse: {m}"),
            ControlPlaneError::InvalidConfig(m) => write!(f, "invalid route config: {m}"),
        }
    }
}

impl std::error::Error for ControlPlaneError {}

pub struct ControlPlane {
    snapshot: Arc<ArcSwap<Snapshot>>,
}

impl ControlPlane {
    /// Waits until the first snapshot loads, retrying while the control plane
    /// is down, then follows changes in the background.
    pub async fn connect(base_url: &str, internal_token: Option<String>) -> Self {
        let client = Client::new(base_url, internal_token);
        let mut retry = Duration::from_secs(1);
        let first = loop {
            match client.fetch(None).await {
                Ok(Some(feed)) => match build(feed) {
                    Ok(snapshot) => break snapshot,
                    Err(e) => error!(error = %e, "first control-plane snapshot refused; retrying"),
                },
                Ok(None) => warn!("control plane had nothing to send; retrying"),
                Err(e) => warn!(error = %e, "waiting for the control plane"),
            }
            sleep(retry).await;
            retry = (retry * 2).min(MAX_RETRY_DELAY);
        };
        info!(
            revision = first.revision,
            routes = first.routes.route_count(),
            "control-plane snapshot loaded"
        );

        let snapshot = Arc::new(ArcSwap::from_pointee(first));
        let follower = snapshot.clone();
        tokio::spawn(async move { follow(client, follower).await });
        Self { snapshot }
    }

    pub fn snapshot(&self) -> Arc<ArcSwap<Snapshot>> {
        self.snapshot.clone()
    }
}

/// Long-polls for each newer revision. A snapshot that fails validation is
/// skipped (the current one stays) and the cursor moves past it, so the next
/// change is still picked up.
async fn follow(client: Client, snapshot: Arc<ArcSwap<Snapshot>>) {
    let mut after = snapshot.load().revision;
    let mut retry = Duration::from_secs(1);
    loop {
        match client.fetch(Some(after)).await {
            Ok(None) => {}
            Ok(Some(feed)) => {
                retry = Duration::from_secs(1);
                let revision = feed.revision;
                match build(feed) {
                    Ok(next) => {
                        let routes = next.routes.route_count();
                        snapshot.store(Arc::new(next));
                        info!(revision, routes, "control-plane snapshot reloaded");
                    }
                    Err(e) => error!(
                        revision,
                        error = %e,
                        "control-plane snapshot refused; keeping current routes"
                    ),
                }
                after = revision;
            }
            Err(e) => {
                warn!(error = %e, "control-plane poll failed; keeping current routes");
                sleep(retry).await;
                retry = (retry * 2).min(MAX_RETRY_DELAY);
            }
        }
    }
}

fn build(feed: GatewayFeed) -> Result<Snapshot, ControlPlaneError> {
    let config = Config {
        services: feed.services,
    };
    config
        .validate()
        .map_err(|e| ControlPlaneError::InvalidConfig(e.to_string()))?;
    Ok(Snapshot {
        revision: feed.revision,
        routes: RouteMap::from_config(&config),
        cors: Arc::new(CorsPolicy::new(feed.cors.allowed_origins)),
    })
}

struct Client {
    http: reqwest::Client,
    url: String,
    internal_token: Option<String>,
}

impl Client {
    fn new(base_url: &str, internal_token: Option<String>) -> Self {
        let http = reqwest::Client::builder()
            .timeout(POLL_TIMEOUT)
            .build()
            .expect("failed to create reqwest client");
        Self {
            http,
            url: format!("{}/internal/gateway", base_url.trim_end_matches('/')),
            internal_token,
        }
    }

    /// The snapshot past `after`, or None when nothing newer came in the wait.
    async fn fetch(&self, after: Option<i64>) -> Result<Option<GatewayFeed>, ControlPlaneError> {
        let mut req = self.http.get(&self.url);
        if let Some(after) = after {
            req = req.query(&[("after", after)]);
        }
        if let Some(token) = &self.internal_token {
            req = req.header(INTERNAL_TOKEN_HEADER, token);
        }
        let resp = req
            .send()
            .await
            .map_err(|e| ControlPlaneError::Network(e.to_string()))?;
        match resp.status().as_u16() {
            204 => Ok(None),
            200 => resp
                .json::<GatewayFeed>()
                .await
                .map(Some)
                .map_err(|e| ControlPlaneError::Decode(e.to_string())),
            other => Err(ControlPlaneError::Status(other)),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn feed(json: serde_json::Value) -> GatewayFeed {
        serde_json::from_value(json).unwrap()
    }

    #[test]
    fn builds_routes_and_cors() {
        let snapshot = build(feed(serde_json::json!({
            "revision": 7,
            "services": {
                "widgets": {
                    "url": "http://widgets:3000",
                    "timeouts": { "read_ms": 60000 },
                    "public": { "routes": [{ "path": "/widgets", "methods": ["GET"] }] }
                }
            },
            "cors": { "allowed_origins": ["https://app.example.com"] }
        })))
        .unwrap();
        assert_eq!(snapshot.revision, 7);
        let (route, _) = snapshot.routes.find_route("/widgets", "GET").unwrap();
        assert_eq!(route.timeouts.read, Some(Duration::from_secs(60)));
        assert_eq!(snapshot.routes.route_count(), 1);
    }

    #[test]
    fn refuses_an_invalid_snapshot() {
        let err = build(feed(serde_json::json!({
            "revision": 8,
            "services": {
                "widgets": { "url": "http://widgets:3000" }
            },
            "cors": { "allowed_origins": [] }
        })))
        .err()
        .expect("a service with no routes is refused");
        assert!(matches!(err, ControlPlaneError::InvalidConfig(_)), "{err}");
    }
}
