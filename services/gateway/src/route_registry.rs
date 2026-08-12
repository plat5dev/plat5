use std::collections::HashMap;
use std::sync::Arc;
use std::time::Duration;

use arc_swap::ArcSwap;
use etcd_client::{Client, GetOptions, WatchOptions};
use tokio::time::sleep;
use tracing::{error, info, warn};

use crate::route_config::{Config, ServiceConfig, ROUTES_PREFIX};
use crate::route_map::RouteMap;

const DEFAULT_ETCD_URL: &str = "http://localhost:2379";

/// Manages the dynamic route registry backed by etcd.
///
/// Routes are loaded from etcd at startup and kept in sync via watches.
/// Each key under `identity/gateway/routes/` contains a JSON `ServiceConfig` blob.
pub struct RouteRegistry {
    route_map: Arc<ArcSwap<RouteMap>>,
}

impl RouteRegistry {
    /// Connect to etcd, load initial routes, and spawn a background watch task.
    ///
    /// If etcd is unreachable, this function returns an error immediately.
    pub async fn connect() -> Result<Self, RouteRegistryError> {
        let etcd_url = std::env::var("ETCD_URL").unwrap_or_else(|_| DEFAULT_ETCD_URL.to_string());

        info!(etcd_url = %etcd_url, "connecting to etcd for route registry");

        let client = Client::connect([etcd_url.as_str()], None)
            .await
            .map_err(|e| RouteRegistryError::Connect(e.to_string()))?;

        let route_map = Self::load_routes(&client).await?;
        let route_map = Arc::new(ArcSwap::from(Arc::new(route_map)));

        info!(
            routes = route_map.load().route_count(),
            "initial route registry loaded"
        );

        // Spawn background watch task
        let watch_route_map = route_map.clone();
        tokio::spawn(async move {
            Self::watch_loop(client, watch_route_map).await;
        });

        Ok(Self { route_map })
    }

    /// Get the current route map.
    pub fn route_map(&self) -> Arc<ArcSwap<RouteMap>> {
        self.route_map.clone()
    }

    /// Load all routes from etcd prefix and build a RouteMap.
    async fn load_routes(client: &Client) -> Result<RouteMap, RouteRegistryError> {
        let mut kv = client.kv_client();

        let resp = kv
            .get(ROUTES_PREFIX, Some(GetOptions::new().with_prefix()))
            .await
            .map_err(|e| RouteRegistryError::Load(e.to_string()))?;

        let mut merged = Config {
            services: HashMap::new(),
        };

        for kv in resp.kvs() {
            let key = String::from_utf8_lossy(kv.key());
            let value = String::from_utf8_lossy(kv.value());

            // Extract service name from key: identity/gateway/routes/{service}
            let service_name = key.strip_prefix(ROUTES_PREFIX).unwrap_or("").to_string();

            if service_name.is_empty() {
                warn!(key = %key, "skipping route key with empty service name");
                continue;
            }

            let service: ServiceConfig = match serde_json::from_str(&value) {
                Ok(c) => c,
                Err(e) => {
                    warn!(
                        service = %service_name,
                        error = %e,
                        "failed to parse route config from etcd, skipping"
                    );
                    continue;
                }
            };

            if merged.services.contains_key(&service_name) {
                warn!(
                    service = %service_name,
                    "duplicate service in etcd route registry, using first encountered"
                );
                continue;
            }
            merged.services.insert(service_name, service);
        }

        merged
            .validate()
            .map_err(|e| RouteRegistryError::InvalidConfig(e.to_string()))?;

        Ok(RouteMap::from_config(&merged))
    }

    /// Background loop: watch etcd for route changes and reload.
    async fn watch_loop(mut client: Client, route_map: Arc<ArcSwap<RouteMap>>) {
        let mut retry_delay = Duration::from_secs(1);
        const MAX_RETRY_DELAY: Duration = Duration::from_secs(60);

        loop {
            match Self::run_watch(&mut client, &route_map).await {
                Ok(()) => {
                    info!("etcd watch stream ended gracefully, reconnecting...");
                    retry_delay = Duration::from_secs(1);
                }
                Err(e) => {
                    error!(error = %e, "etcd watch error, will retry");
                }
            }

            sleep(retry_delay).await;
            retry_delay = std::cmp::min(retry_delay * 2, MAX_RETRY_DELAY);
        }
    }

    /// Establish a watch and wait for events.
    async fn run_watch(
        client: &mut Client,
        route_map: &Arc<ArcSwap<RouteMap>>,
    ) -> Result<(), RouteRegistryError> {
        let (mut watcher, mut stream) = client
            .watch(ROUTES_PREFIX, Some(WatchOptions::new().with_prefix()))
            .await
            .map_err(|e| RouteRegistryError::Watch(e.to_string()))?;

        info!("etcd watch established for route registry");

        while let Some(resp) = stream
            .message()
            .await
            .map_err(|e| RouteRegistryError::Watch(e.to_string()))?
        {
            if resp.canceled() {
                info!("etcd watch canceled");
                break;
            }

            // Any event (create, modify, delete) triggers a full reload.
            // Route count is small; full reload is simpler than incremental.
            info!("route registry change detected, reloading...");

            match Self::load_routes(client).await {
                Ok(new_map) => {
                    let count = new_map.route_count();
                    route_map.store(Arc::new(new_map));
                    info!(routes = count, "route registry reloaded");
                }
                Err(e) => {
                    error!(error = %e, "failed to reload route registry, keeping current routes");
                }
            }
        }

        let _ = watcher.cancel().await;
        Ok(())
    }
}

#[derive(Debug)]
pub enum RouteRegistryError {
    Connect(String),
    Load(String),
    InvalidConfig(String),
    Watch(String),
}

impl std::fmt::Display for RouteRegistryError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            RouteRegistryError::Connect(msg) => write!(f, "failed to connect to etcd: {}", msg),
            RouteRegistryError::Load(msg) => write!(f, "failed to load routes from etcd: {}", msg),
            RouteRegistryError::InvalidConfig(msg) => write!(f, "invalid route config: {}", msg),
            RouteRegistryError::Watch(msg) => write!(f, "etcd watch error: {}", msg),
        }
    }
}

impl std::error::Error for RouteRegistryError {}
