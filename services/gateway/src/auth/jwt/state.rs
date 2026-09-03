use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::Duration;

use arc_swap::ArcSwapOption;
use jsonwebtoken::jwk::JwkSet;
use tokio::sync::{broadcast, Mutex};
use tracing::{info, warn};

const FETCH_TIMEOUT_SECS: u64 = 10;
const RETRY_INTERVAL_SECS: u64 = 2;
const REFRESH_INTERVAL_SECS: u64 = 15 * 60;

type JwksFetchTx = broadcast::Sender<Result<Arc<JwkSet>, String>>;

#[derive(Clone)]
pub struct JwtValidatorState {
    issuer: Arc<str>,
    jwks_uri: Arc<str>,
    allowed_audiences: Arc<[String]>,
    jwks: Arc<ArcSwapOption<JwkSet>>,
    client: reqwest::Client,
    /// In-flight cold-path fetch. Waiters subscribe; the network call runs without this lock.
    in_flight: Arc<Mutex<Option<JwksFetchTx>>>,
    refresh_started: Arc<AtomicBool>,
}

impl JwtValidatorState {
    pub fn new(issuer: String, jwks_uri: String, allowed_audiences: Vec<String>) -> Self {
        let client = reqwest::Client::builder()
            .timeout(Duration::from_secs(FETCH_TIMEOUT_SECS))
            .build()
            .expect("failed to create reqwest client");

        Self {
            issuer: issuer.into(),
            jwks_uri: jwks_uri.into(),
            allowed_audiences: allowed_audiences.into(),
            jwks: Arc::new(ArcSwapOption::empty()),
            client,
            in_flight: Arc::new(Mutex::new(None)),
            refresh_started: Arc::new(AtomicBool::new(false)),
        }
    }

    pub async fn get_jwks(&self) -> Result<JwkSet, reqwest::Error> {
        if let Some(jwks) = self.jwks.load_full() {
            return Ok((*jwks).clone());
        }

        let waiter = {
            let mut slot = self.in_flight.lock().await;
            if let Some(jwks) = self.jwks.load_full() {
                return Ok((*jwks).clone());
            }
            if let Some(tx) = slot.as_ref() {
                Some(tx.subscribe())
            } else {
                let (tx, _) = broadcast::channel(1);
                *slot = Some(tx);
                None
            }
        };

        if let Some(mut rx) = waiter {
            match rx.recv().await {
                Ok(Ok(jwks)) => return Ok((*jwks).clone()),
                Ok(Err(_)) | Err(_) => {
                    // Fetcher failed or lagged; try again below if cache still empty.
                }
            }
            if let Some(jwks) = self.jwks.load_full() {
                return Ok((*jwks).clone());
            }
            return self.fetch_and_store().await;
        }

        let result = self.fetch_and_store().await;
        {
            let mut slot = self.in_flight.lock().await;
            if let Some(tx) = slot.take() {
                match &result {
                    Ok(j) => {
                        let _ = tx.send(Ok(Arc::new(j.clone())));
                    }
                    Err(err) => {
                        let _ = tx.send(Err(err.to_string()));
                    }
                }
            }
        }
        if result.is_ok() {
            self.try_start_refresh_task();
        }
        result
    }

    async fn fetch_and_store(&self) -> Result<JwkSet, reqwest::Error> {
        let new_jwks = fetch_jwks(&self.client, &self.jwks_uri).await?;
        self.jwks.store(Some(Arc::new(new_jwks.clone())));
        Ok(new_jwks)
    }

    pub fn get_issuer(&self) -> String {
        self.issuer.to_string()
    }

    pub fn get_allowed_audiences(&self) -> &[String] {
        &self.allowed_audiences
    }

    /// Check if JWKS is loaded (non-blocking).
    pub fn is_ready(&self) -> bool {
        self.jwks.load_full().is_some()
    }

    /// Attempt to fetch JWKS immediately. On failure the cache stays empty
    /// and the background task retries every 2s until the first success,
    /// then every 15 minutes.
    pub async fn initialize(&self) -> Result<(), reqwest::Error> {
        match fetch_jwks(&self.client, &self.jwks_uri).await {
            Ok(new_jwks) => {
                self.jwks.store(Some(Arc::new(new_jwks)));
                info!(jwks_uri = %self.jwks_uri, "jwks loaded successfully on startup");
            }
            Err(err) => {
                warn!(
                    jwks_uri = %self.jwks_uri,
                    error_kind = crate::error::ErrorKind::Network.as_str(),
                    error_message = %err,
                    "jwks startup fetch failed; will retry in background"
                );
                self.try_start_refresh_task();
                return Err(err);
            }
        }

        self.try_start_refresh_task();
        Ok(())
    }

    fn try_start_refresh_task(&self) {
        if self
            .refresh_started
            .compare_exchange(false, true, Ordering::SeqCst, Ordering::Relaxed)
            .is_ok()
        {
            let state = self.clone();
            tokio::spawn(async move {
                loop {
                    let delay = if state.is_ready() {
                        Duration::from_secs(REFRESH_INTERVAL_SECS)
                    } else {
                        Duration::from_secs(RETRY_INTERVAL_SECS)
                    };
                    tokio::time::sleep(delay).await;
                    if let Err(err) = state.refresh_jwks().await {
                        if state.is_ready() {
                            warn!(
                                jwks_uri = %state.jwks_uri,
                                error_kind = crate::error::ErrorKind::Network.as_str(),
                                error_message = %err,
                                "jwks background refresh failed"
                            );
                        } else {
                            warn!(
                                jwks_uri = %state.jwks_uri,
                                error_kind = crate::error::ErrorKind::Network.as_str(),
                                error_message = %err,
                                "jwks retry fetch failed"
                            );
                        }
                    }
                }
            });
        }
    }

    async fn refresh_jwks(&self) -> Result<(), reqwest::Error> {
        let new_jwks = fetch_jwks(&self.client, &self.jwks_uri).await?;
        let was_empty = self.jwks.load_full().is_none();
        self.jwks.store(Some(Arc::new(new_jwks)));
        if was_empty {
            info!(jwks_uri = %self.jwks_uri, "jwks loaded successfully");
        } else {
            info!(jwks_uri = %self.jwks_uri, "jwks refreshed successfully");
        }
        Ok(())
    }
}

async fn fetch_jwks(client: &reqwest::Client, uri: &str) -> reqwest::Result<JwkSet> {
    let res = client.get(uri).send().await?;
    res.json::<JwkSet>().await
}
