//! What the gateway runs on: routes, rate limits and CORS from the control plane.
//!
//! At boot the gateway waits for its first snapshot. It then long-polls
//! `GET {CONTROL_PLANE_URL}/internal/gateway?after={revision}` and swaps the
//! whole snapshot on every change. While the control plane is down it keeps
//! serving the last snapshot. A snapshot it can't parse or build is refused whole:
//! it keeps the current one and reports the refusal on its next poll. Contract:
//! docs/reference/gateway.md#control-plane.

use std::collections::BTreeMap;
use std::sync::Arc;
use std::time::Duration;

use arc_swap::ArcSwap;
use serde::Deserialize;
use serde_json::Value;
use tokio::time::sleep;
use tracing::{error, info, warn};

use crate::auth::{parse_claim_path, NameClaims};
use crate::gateway::CorsPolicy;
use crate::kinds::{KindDoc, Kinds};
use crate::metrics;
use crate::route_map::{Policy, PolicyDoc, RouteMap, ServiceDoc};

/// Longer than the control plane's long-poll wait, so an idle poll ends with 204.
const POLL_TIMEOUT: Duration = Duration::from_secs(40);
const MAX_RETRY_DELAY: Duration = Duration::from_secs(10);
/// The policy of every route that names none, and of route checks.
const DEFAULT_POLICY: &str = "default";
/// The most of a refusal's reason a poll reports.
const MAX_REASON_CHARS: usize = 512;
/// One control-plane revision, swapped whole.
pub struct Snapshot {
    pub revision: i64,
    pub routes: RouteMap,
    pub cors: Arc<CorsPolicy>,
    /// `users.name.claims`, each as path segments: the JWT claims that may
    /// hold a person's name, in order.
    pub name_claims: Arc<NameClaims>,
    /// Where the gateway answers route checks, and the `default` policy it
    /// counts each call against.
    pub route_checks: RouteChecks,
}

/// `route_checks` (docs/reference/gateway.md#route-checks).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct RouteChecks {
    pub path: String,
    pub default: Policy,
}

#[derive(Deserialize)]
struct GatewayFeed {
    revision: i64,
    kinds: BTreeMap<String, KindDoc>,
    services: BTreeMap<String, ServiceDoc>,
    rate_limits: BTreeMap<String, PolicyDoc>,
    cors: CorsFeed,
    users: UsersFeed,
    route_checks: RouteChecksFeed,
}

#[derive(Deserialize)]
struct RouteChecksFeed {
    path: String,
}

#[derive(Deserialize)]
struct CorsFeed {
    allowed_origins: Vec<String>,
    allowed_headers: Vec<String>,
}

#[derive(Deserialize)]
struct UsersFeed {
    name: NameFeed,
}

/// `users.name`. The gateway reads only `claims`; `sync` is identity's.
#[derive(Deserialize)]
struct NameFeed {
    claims: Vec<String>,
}

#[derive(Debug)]
pub enum ControlPlaneError {
    Network(String),
    Status(u16),
    Decode(String),
}

impl std::fmt::Display for ControlPlaneError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            ControlPlaneError::Network(m) => write!(f, "control plane unreachable: {m}"),
            ControlPlaneError::Status(s) => write!(f, "control plane answered {s}"),
            ControlPlaneError::Decode(m) => write!(f, "control plane answer did not parse: {m}"),
        }
    }
}

impl std::error::Error for ControlPlaneError {}

/// Where this replica stands, sent on every poll.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Report {
    pub replica: String,
    /// The revision it serves; `0` before its first.
    pub loaded: i64,
    /// The newest revision it was served, when it refused that one, and why.
    pub refused: Option<(i64, String)>,
}

impl Report {
    pub fn new(replica: String) -> Self {
        Self {
            replica,
            loaded: 0,
            refused: None,
        }
    }

    /// The newest revision it was served, loaded or refused: the poll's `after`.
    pub fn cursor(&self) -> i64 {
        self.refused
            .as_ref()
            .map_or(self.loaded, |(r, _)| (*r).max(self.loaded))
    }

    pub fn loaded(&mut self, revision: i64) {
        self.loaded = revision;
        self.refused = None;
        metrics::record_config_revision(revision);
    }

    /// Logs and counts a refusal once per revision.
    pub fn refused(&mut self, revision: i64, reason: String) {
        if self.refused.as_ref().is_some_and(|(r, _)| *r == revision) {
            return;
        }
        error!(
            revision,
            error = %reason,
            "control-plane snapshot refused; keeping current routes"
        );
        metrics::record_config_refused(revision);
        self.refused = Some((revision, reason.chars().take(MAX_REASON_CHARS).collect()));
    }

    /// The query a poll carries.
    pub fn query(&self, after: Option<i64>) -> Vec<(&'static str, String)> {
        let mut q = Vec::new();
        if let Some(after) = after {
            q.push(("after", after.to_string()));
        }
        q.push(("replica", self.replica.clone()));
        q.push(("loaded", self.loaded.to_string()));
        if let Some((revision, reason)) = &self.refused {
            q.push(("refused", revision.to_string()));
            q.push(("reason", reason.clone()));
        }
        q
    }
}

pub struct ControlPlane {
    snapshot: Arc<ArcSwap<Snapshot>>,
}

impl ControlPlane {
    /// Waits until the first snapshot loads, retrying while the control plane
    /// is down, then follows changes in the background.
    pub async fn connect(base_url: &str, replica: String) -> Self {
        let client = Client::new(base_url);
        let mut report = Report::new(replica);
        let mut retry = Duration::from_secs(1);
        let first = loop {
            // After a refusal, wait for a newer revision.
            let after = report.refused.as_ref().map(|(r, _)| *r);
            match client.fetch(&report.query(after)).await {
                Ok(Some(feed)) => match revision_of(&feed) {
                    Some(revision) => match build(feed) {
                        Ok(snapshot) => break snapshot,
                        Err(e) => report.refused(revision, e),
                    },
                    None => {
                        error!("control-plane answer has no revision; retrying");
                        sleep(retry).await;
                    }
                },
                Ok(None) => {}
                Err(e) => {
                    warn!(error = %e, "waiting for the control plane");
                    sleep(retry).await;
                    retry = (retry * 2).min(MAX_RETRY_DELAY);
                }
            }
        };
        report.loaded(first.revision);
        info!(
            revision = first.revision,
            routes = first.routes.route_count(),
            "control-plane snapshot loaded"
        );

        let snapshot = Arc::new(ArcSwap::from_pointee(first));
        let follower = snapshot.clone();
        tokio::spawn(async move { follow(client, report, follower).await });
        Self { snapshot }
    }

    pub fn snapshot(&self) -> Arc<ArcSwap<Snapshot>> {
        self.snapshot.clone()
    }
}

/// Long-polls for each newer revision. A refused snapshot is skipped (the current
/// one stays) and the cursor moves past it, so the next change is still picked up.
async fn follow(client: Client, mut report: Report, snapshot: Arc<ArcSwap<Snapshot>>) {
    let mut retry = Duration::from_secs(1);
    loop {
        match client.fetch(&report.query(Some(report.cursor()))).await {
            Ok(None) => {}
            Ok(Some(feed)) => {
                retry = Duration::from_secs(1);
                let Some(revision) = revision_of(&feed) else {
                    error!("control-plane answer has no revision; keeping current routes");
                    sleep(retry).await;
                    continue;
                };
                match build(feed) {
                    Ok(next) => {
                        let routes = next.routes.route_count();
                        snapshot.store(Arc::new(next));
                        report.loaded(revision);
                        info!(revision, routes, "control-plane snapshot reloaded");
                    }
                    Err(e) => report.refused(revision, e),
                }
            }
            Err(e) => {
                warn!(error = %e, "control-plane poll failed; keeping current routes");
                sleep(retry).await;
                retry = (retry * 2).min(MAX_RETRY_DELAY);
            }
        }
    }
}

/// The revision a feed answer names, if it names one: a refused snapshot is
/// reported by it.
fn revision_of(feed: &Value) -> Option<i64> {
    feed.get("revision").and_then(Value::as_i64)
}

/// Parses a feed answer and builds it, or refuses it whole with the reason.
pub fn build(feed: Value) -> Result<Snapshot, String> {
    let feed: GatewayFeed = serde_json::from_value(feed).map_err(|e| e.to_string())?;
    let kinds = Kinds::from_feed(&feed.kinds);
    let routes = RouteMap::from_feed(&feed.services, &feed.rate_limits, &kinds)?;
    let default = feed
        .rate_limits
        .get(DEFAULT_POLICY)
        .ok_or("rate_limits has no default policy")?;
    let route_checks = RouteChecks {
        path: feed.route_checks.path,
        default: Policy {
            name: DEFAULT_POLICY.to_string(),
            requests: default.requests,
            window_seconds: default.window_seconds,
            count_at: None,
        },
    };
    Ok(Snapshot {
        revision: feed.revision,
        routes,
        cors: Arc::new(CorsPolicy::new(
            feed.cors.allowed_origins,
            &feed.cors.allowed_headers,
        )),
        name_claims: Arc::new(
            feed.users
                .name
                .claims
                .iter()
                .map(|c| parse_claim_path(c))
                .collect(),
        ),
        route_checks,
    })
}

struct Client {
    http: reqwest::Client,
    url: String,
}

impl Client {
    fn new(base_url: &str) -> Self {
        let http = reqwest::Client::builder()
            .timeout(POLL_TIMEOUT)
            .build()
            .expect("failed to create reqwest client");
        Self {
            http,
            url: format!("{}/internal/gateway", base_url.trim_end_matches('/')),
        }
    }

    /// The snapshot the query asks for, or None when nothing newer came in the wait.
    async fn fetch(&self, query: &[(&str, String)]) -> Result<Option<Value>, ControlPlaneError> {
        let resp = self
            .http
            .get(&self.url)
            .query(query)
            .send()
            .await
            .map_err(|e| ControlPlaneError::Network(e.to_string()))?;
        match resp.status().as_u16() {
            204 => Ok(None),
            200 => resp
                .json::<Value>()
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
    use serde_json::json;
    use std::time::Duration;

    fn feed(services: Value) -> Value {
        json!({
            "revision": 7,
            "kinds": { "organization": { "path": "/orgs/{organization}", "list_permission": "organization:read" } },
            "services": services,
            "rate_limits": { "default": { "requests": 60, "window_seconds": 30 } },
            "cors": { "allowed_origins": ["https://app.example.com"], "allowed_headers": [] },
            "users": { "name": { "claims": ["profile.full_name", "name"], "sync": "each_sign_in" } },
            "route_checks": { "path": "/access/routes" }
        })
    }

    fn widgets(timeouts: Value) -> Value {
        json!({ "widgets": {
            "url": "http://widgets:3000", "timeouts": timeouts, "max_body_bytes": 2048,
            "routes": [{ "path": "/widgets", "method": "GET", "scope": "public", "upstream": "/widgets",
                         "required_permissions": null, "rate_limit": [], "audit": false,
                         "allow_deleted": false }]
        }})
    }

    #[test]
    fn builds_routes_cors_and_the_name_claims() {
        let snapshot = build(feed(widgets(json!({ "connect": "1s", "read": "60s" })))).unwrap();
        assert_eq!(snapshot.revision, 7);
        assert_eq!(
            *snapshot.name_claims,
            vec![vec!["profile", "full_name"], vec!["name"]]
        );
        let m = snapshot.routes.find_route("/widgets", "GET").unwrap();
        assert_eq!(m.route.timeouts.read, Duration::from_secs(60));
        assert_eq!(m.route.max_body_bytes, 2048);
        assert_eq!(snapshot.routes.route_count(), 1);
        assert_eq!(snapshot.route_checks.path, "/access/routes");
        assert_eq!(
            snapshot.route_checks.default,
            Policy {
                name: "default".into(),
                requests: 60,
                window_seconds: 30,
                count_at: None
            }
        );
    }

    #[test]
    fn refuses_a_snapshot_it_cannot_parse_or_build() {
        let mut no_revision = feed(json!({}));
        no_revision.as_object_mut().unwrap().remove("revision");
        let mut no_rate_limits = feed(json!({}));
        no_rate_limits
            .as_object_mut()
            .unwrap()
            .remove("rate_limits");
        let mut no_route_checks = feed(json!({}));
        no_route_checks
            .as_object_mut()
            .unwrap()
            .remove("route_checks");
        let mut no_default = feed(json!({}));
        no_default["rate_limits"] = json!({});
        for bad in [
            no_revision,
            no_rate_limits,
            no_route_checks,
            no_default,
            feed(json!({ "w": { "url": "http://w:1" } })),
            feed(widgets(json!({ "connect": "1s" }))),
            feed(widgets(json!({ "connect": "fast", "read": "1s" }))),
        ] {
            assert!(build(bad.clone()).is_err(), "{bad}");
        }
    }

    /// Every config the control plane accepted builds here: the catalog fixture and
    /// each plan case's feed. A case with only `refused.json` was refused at apply.
    #[test]
    fn every_accepted_fixture_feed_builds() {
        let fixtures = concat!(env!("CARGO_MANIFEST_DIR"), "/../control-plane/fixtures");
        let mut feeds = vec![std::path::PathBuf::from(format!(
            "{fixtures}/catalog/gateway-feed.json"
        ))];
        let mut cases: Vec<_> = std::fs::read_dir(format!("{fixtures}/plans"))
            .expect("plan fixtures")
            .map(|e| e.unwrap().path())
            .collect();
        cases.sort();
        let mut refused = 0;
        for case in cases {
            let feed = case.join("gateway-feed.json");
            if feed.exists() {
                feeds.push(feed);
            } else {
                assert!(case.join("refused.json").exists(), "{}", case.display());
                refused += 1;
            }
        }
        assert!(feeds.len() >= 10 && refused >= 1, "{} feeds", feeds.len());

        for path in feeds {
            let text = std::fs::read_to_string(&path).unwrap();
            let feed: Value = serde_json::from_str(&text).unwrap();
            let want: usize = feed["services"]
                .as_object()
                .unwrap()
                .values()
                .map(|s| s["routes"].as_array().unwrap().len())
                .sum();
            let snapshot =
                build(feed.clone()).unwrap_or_else(|e| panic!("{}: {e}", path.display()));
            assert_eq!(snapshot.routes.route_count(), want, "{}", path.display());
            // Every route is reachable at its own path.
            for route in snapshot.routes.routes() {
                let path_of = route
                    .path
                    .split('/')
                    .map(|s| if s.starts_with('{') { "01x" } else { s })
                    .collect::<Vec<_>>()
                    .join("/");
                let m = snapshot.routes.find_route(&path_of, &route.method).unwrap();
                assert_eq!(m.route.path, route.path, "{}", path.display());
            }
        }
    }

    #[test]
    fn a_poll_reports_what_it_loaded_and_what_it_refused() {
        let mut report = Report::new("gw-1".into());
        assert_eq!(
            report.query(None),
            vec![("replica", "gw-1".to_string()), ("loaded", "0".to_string())]
        );
        report.loaded(41);
        report.refused(42, "x".repeat(MAX_REASON_CHARS + 10));
        assert_eq!(report.cursor(), 42);
        let q = report.query(Some(report.cursor()));
        assert_eq!(q[0], ("after", "42".to_string()));
        assert_eq!(q[2], ("loaded", "41".to_string()));
        assert_eq!(q[3], ("refused", "42".to_string()));
        assert_eq!(q[4].1.len(), MAX_REASON_CHARS);
        // A newer revision that loads clears the refusal.
        report.loaded(43);
        assert_eq!(report.cursor(), 43);
        assert_eq!(report.query(None).len(), 2);
    }
}
