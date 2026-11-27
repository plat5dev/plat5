use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

use arc_swap::ArcSwapOption;
use jsonwebtoken::jwk::JwkSet;
use serde::Deserialize;
use tokio::sync::{broadcast, watch, Mutex};
use tracing::{info, warn};

const FETCH_TIMEOUT_SECS: u64 = 10;
const RETRY_INTERVAL_SECS: u64 = 2;
const REFRESH_INTERVAL_SECS: u64 = 15 * 60;
const UNKNOWN_KID_REFRESH_COOLDOWN_SECS: u64 = 30;

type JwksFetchTx = broadcast::Sender<Result<Arc<JwkSet>, String>>;

struct FetchState {
    in_flight: Option<JwksFetchTx>,
    last_on_demand_refresh: Option<Instant>,
}

#[derive(Deserialize)]
struct OidcDiscoveryDoc {
    issuer: String,
    jwks_uri: String,
}

#[derive(Debug, Clone)]
pub struct JwksUnavailable(pub String);

impl std::fmt::Display for JwksUnavailable {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.0)
    }
}

impl std::error::Error for JwksUnavailable {}

#[derive(Clone)]
pub struct IssuerJwks {
    issuer: Arc<str>,
    explicit_jwks_uri: Option<Arc<str>>,
    discovered_jwks_uri: Arc<ArcSwapOption<String>>,
    jwks: Arc<ArcSwapOption<JwkSet>>,
    client: reqwest::Client,
    fetch_state: Arc<Mutex<FetchState>>,
    refresh_cooldown: Duration,
    refresh_started: Arc<AtomicBool>,
}

impl IssuerJwks {
    pub fn new(issuer: String, explicit_jwks_uri: Option<String>) -> Self {
        Self::with_refresh_cooldown(
            issuer,
            explicit_jwks_uri,
            Duration::from_secs(UNKNOWN_KID_REFRESH_COOLDOWN_SECS),
        )
    }

    pub fn with_refresh_cooldown(
        issuer: String,
        explicit_jwks_uri: Option<String>,
        refresh_cooldown: Duration,
    ) -> Self {
        let client = reqwest::Client::builder()
            .timeout(Duration::from_secs(FETCH_TIMEOUT_SECS))
            .build()
            .expect("failed to create reqwest client");
        Self {
            issuer: issuer.into(),
            explicit_jwks_uri: explicit_jwks_uri.map(Into::into),
            discovered_jwks_uri: Arc::new(ArcSwapOption::empty()),
            jwks: Arc::new(ArcSwapOption::empty()),
            client,
            fetch_state: Arc::new(Mutex::new(FetchState {
                in_flight: None,
                last_on_demand_refresh: None,
            })),
            refresh_cooldown,
            refresh_started: Arc::new(AtomicBool::new(false)),
        }
    }

    #[cfg(test)]
    pub fn with_static_jwks(issuer: String, jwks: JwkSet) -> Self {
        let s = Self::new(issuer, None);
        s.jwks.store(Some(Arc::new(jwks)));
        s
    }

    pub fn is_ready(&self) -> bool {
        self.jwks.load_full().is_some()
    }

    pub async fn get_jwks(&self) -> Result<Arc<JwkSet>, JwksUnavailable> {
        if let Some(jwks) = self.jwks.load_full() {
            return Ok(jwks);
        }

        let waiter = {
            let mut state = self.fetch_state.lock().await;
            if let Some(jwks) = self.jwks.load_full() {
                return Ok(jwks);
            }
            if let Some(tx) = state.in_flight.as_ref() {
                Some(tx.subscribe())
            } else {
                let (tx, _) = broadcast::channel(1);
                state.in_flight = Some(tx);
                None
            }
        };

        if let Some(mut rx) = waiter {
            return match rx.recv().await {
                Ok(Ok(jwks)) => Ok(jwks),
                Ok(Err(msg)) => Err(JwksUnavailable(msg)),
                Err(_) => match self.jwks.load_full() {
                    Some(jwks) => Ok(jwks),
                    None => Err(JwksUnavailable("in-flight jwks fetch ended".to_string())),
                },
            };
        }

        let result = self.fetch_and_store().await.map_err(JwksUnavailable);
        {
            let mut state = self.fetch_state.lock().await;
            if let Some(tx) = state.in_flight.take() {
                match &result {
                    Ok(j) => {
                        let _ = tx.send(Ok(Arc::clone(j)));
                    }
                    Err(err) => {
                        let _ = tx.send(Err(err.0.clone()));
                    }
                }
            }
        }
        result
    }

    /// Singleflight-coalesced, cooldown-throttled JWKS refetch on an unknown `kid`.
    pub async fn refresh_for_unknown_kid(&self, kid: &str) -> Option<Arc<JwkSet>> {
        let waiter = {
            let mut state = self.fetch_state.lock().await;
            if let Some(jwks) = self.jwks.load_full() {
                if jwks.find(kid).is_some() {
                    return Some(jwks);
                }
            }
            if let Some(tx) = state.in_flight.as_ref() {
                Some(tx.subscribe())
            } else {
                if let Some(last) = state.last_on_demand_refresh {
                    if last.elapsed() < self.refresh_cooldown {
                        return None;
                    }
                }
                state.last_on_demand_refresh = Some(Instant::now());
                let (tx, _) = broadcast::channel(1);
                state.in_flight = Some(tx);
                None
            }
        };

        if let Some(mut rx) = waiter {
            return match rx.recv().await {
                Ok(Ok(jwks)) => Some(jwks),
                Ok(Err(_)) => None,
                Err(_) => self.jwks.load_full(),
            };
        }

        let result = self.fetch_and_store().await;
        {
            let mut state = self.fetch_state.lock().await;
            if let Some(tx) = state.in_flight.take() {
                match &result {
                    Ok(j) => {
                        let _ = tx.send(Ok(Arc::clone(j)));
                    }
                    Err(err) => {
                        let _ = tx.send(Err(err.clone()));
                    }
                }
            }
        }
        match result {
            Ok(jwks) => {
                info!(issuer = %self.issuer, kid = %kid, "control-plane jwks refreshed after unknown kid");
                Some(jwks)
            }
            Err(err) => {
                warn!(
                    issuer = %self.issuer,
                    kid = %kid,
                    error = %err,
                    "control-plane jwks on-demand refresh failed; keeping cached keys"
                );
                None
            }
        }
    }

    pub fn spawn_refresh(&self, mut shutdown: watch::Receiver<bool>) {
        if self
            .refresh_started
            .compare_exchange(false, true, Ordering::SeqCst, Ordering::Relaxed)
            .is_err()
        {
            return;
        }
        let this = self.clone();
        tokio::spawn(async move {
            // Eager initial fetch on startup.
            match this.fetch_and_store().await {
                Ok(_) => info!(issuer = %this.issuer, "control-plane jwks loaded on startup"),
                Err(err) => warn!(
                    issuer = %this.issuer,
                    error = %err,
                    "control-plane jwks startup fetch failed; will retry in background"
                ),
            }
            loop {
                let delay = if this.is_ready() {
                    Duration::from_secs(REFRESH_INTERVAL_SECS)
                } else {
                    Duration::from_secs(RETRY_INTERVAL_SECS)
                };
                tokio::select! {
                    _ = wait_shutdown(&mut shutdown) => break,
                    _ = tokio::time::sleep(delay) => {}
                }
                let was_empty = !this.is_ready();
                match this.fetch_and_store().await {
                    Ok(_) => {
                        if was_empty {
                            info!(issuer = %this.issuer, "control-plane jwks loaded");
                        } else {
                            info!(issuer = %this.issuer, "control-plane jwks refreshed");
                        }
                    }
                    Err(err) => {
                        warn!(
                            issuer = %this.issuer,
                            error = %err,
                            "control-plane jwks background refresh failed"
                        );
                    }
                }
            }
        });
    }

    async fn resolve_jwks_uri(&self) -> Result<String, String> {
        if let Some(ref uri) = self.explicit_jwks_uri {
            return Ok(uri.to_string());
        }
        if let Some(uri) = self.discovered_jwks_uri.load_full() {
            return Ok((*uri).clone());
        }
        let discovery_url = format!(
            "{}/.well-known/openid-configuration",
            self.issuer.trim_end_matches('/')
        );
        let resp = self
            .client
            .get(&discovery_url)
            .send()
            .await
            .map_err(|e| format!("oidc discovery {discovery_url}: {e}"))?
            .error_for_status()
            .map_err(|e| format!("oidc discovery {discovery_url}: {e}"))?;
        let doc: OidcDiscoveryDoc = resp
            .json()
            .await
            .map_err(|e| format!("decode oidc discovery {discovery_url}: {e}"))?;
        if doc.issuer != self.issuer.as_ref() {
            return Err(format!(
                "oidc discovery issuer mismatch: expected {:?}, got {:?}",
                self.issuer, doc.issuer
            ));
        }
        if !(doc.jwks_uri.starts_with("https://") || doc.jwks_uri.starts_with("http://")) {
            return Err(format!(
                "oidc discovery jwks_uri is not http(s): {:?}",
                doc.jwks_uri
            ));
        }
        self.discovered_jwks_uri
            .store(Some(Arc::new(doc.jwks_uri.clone())));
        Ok(doc.jwks_uri)
    }

    async fn fetch_and_store(&self) -> Result<Arc<JwkSet>, String> {
        let uri = self.resolve_jwks_uri().await?;
        let res = self
            .client
            .get(&uri)
            .send()
            .await
            .map_err(|e| format!("fetch jwks {uri}: {e}"))?
            .error_for_status()
            .map_err(|e| format!("fetch jwks {uri}: {e}"))?;
        let jwks = res
            .json::<JwkSet>()
            .await
            .map_err(|e| format!("decode jwks {uri}: {e}"))?;
        let jwks = Arc::new(jwks);
        self.jwks.store(Some(Arc::clone(&jwks)));
        Ok(jwks)
    }
}

async fn wait_shutdown(rx: &mut watch::Receiver<bool>) {
    if *rx.borrow() {
        return;
    }
    while rx.changed().await.is_ok() {
        if *rx.borrow() {
            return;
        }
    }
}
