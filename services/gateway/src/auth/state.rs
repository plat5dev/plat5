use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::Duration;

use jsonwebtoken::jwk::JwkSet;
use tokio::sync::Mutex;
use tracing::{info, warn};

const FETCH_TIMEOUT_SECS: u64 = 10;
const REFRESH_INTERVAL_SECS: u64 = 15 * 60;

#[derive(Clone)]
pub struct JwtValidatorState {
    issuer: String,
    jwks_uri: String,
    allowed_audiences: Vec<String>,
    jwks: Arc<Mutex<Option<JwkSet>>>,
    client: reqwest::Client,
    refresh_started: Arc<AtomicBool>,
}

impl JwtValidatorState {
    pub fn new(issuer: String, jwks_uri: String, allowed_audiences: Vec<String>) -> Self {
        let client = reqwest::Client::builder()
            .timeout(Duration::from_secs(FETCH_TIMEOUT_SECS))
            .build()
            .expect("failed to create reqwest client");

        Self {
            issuer,
            jwks_uri,
            allowed_audiences,
            jwks: Arc::new(Mutex::new(None)),
            client,
            refresh_started: Arc::new(AtomicBool::new(false)),
        }
    }

    pub async fn get_jwks(&self) -> Result<JwkSet, reqwest::Error> {
        let mut jwks_guard = self.jwks.lock().await;

        match &*jwks_guard {
            Some(jwks) => Ok(jwks.clone()),
            None => {
                let new_jwks = fetch_jwks(&self.client, &self.jwks_uri).await?;
                *jwks_guard = Some(new_jwks.clone());
                drop(jwks_guard);

                self.try_start_refresh_task();

                Ok(new_jwks)
            }
        }
    }

    pub fn get_issuer(&self) -> String {
        self.issuer.clone()
    }

    pub fn get_allowed_audiences(&self) -> &[String] {
        &self.allowed_audiences
    }

    /// Check if JWKS is loaded (non-blocking)
    pub fn is_ready(&self) -> bool {
        self.jwks
            .try_lock()
            .map(|guard| guard.is_some())
            .unwrap_or(false)
    }

    /// Attempt to fetch JWKS immediately. On failure the cache stays empty
    /// but the background refresh task is still started so we recover later.
    pub async fn initialize(&self) -> Result<(), reqwest::Error> {
        match fetch_jwks(&self.client, &self.jwks_uri).await {
            Ok(new_jwks) => {
                let mut jwks_guard = self.jwks.lock().await;
                *jwks_guard = Some(new_jwks);
                info!(jwks_uri = %self.jwks_uri, "jwks loaded successfully on startup");
            }
            Err(err) => {
                warn!(
                    jwks_uri = %self.jwks_uri,
                    error_kind = crate::error::ErrorKind::Network.as_str(),
                    error_message = %err,
                    "jwks startup fetch failed; will retry in background"
                );
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
                let mut interval =
                    tokio::time::interval(Duration::from_secs(REFRESH_INTERVAL_SECS));
                interval.tick().await; // skip immediate tick
                loop {
                    interval.tick().await;
                    if let Err(err) = state.refresh_jwks().await {
                        warn!(
                            jwks_uri = %state.jwks_uri,
                            error_kind = crate::error::ErrorKind::Network.as_str(),
                            error_message = %err,
                            "jwks background refresh failed"
                        );
                    }
                }
            });
        }
    }

    async fn refresh_jwks(&self) -> Result<(), reqwest::Error> {
        let new_jwks = fetch_jwks(&self.client, &self.jwks_uri).await?;
        let mut jwks_guard = self.jwks.lock().await;
        *jwks_guard = Some(new_jwks);
        info!(jwks_uri = %self.jwks_uri, "jwks refreshed successfully");
        Ok(())
    }
}

async fn fetch_jwks(client: &reqwest::Client, uri: &str) -> reqwest::Result<JwkSet> {
    let res = client.get(uri).send().await?;
    res.json::<JwkSet>().await
}
