use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

use arc_swap::ArcSwapOption;
use jsonwebtoken::jwk::JwkSet;
use tokio::sync::{broadcast, Mutex};
use tracing::{info, warn};

use super::Audiences;

const FETCH_TIMEOUT_SECS: u64 = 10;
const RETRY_INTERVAL_SECS: u64 = 2;
const REFRESH_INTERVAL_SECS: u64 = 15 * 60;
const UNKNOWN_KID_REFRESH_COOLDOWN_SECS: u64 = 30;

type JwksFetchTx = broadcast::Sender<Result<Arc<JwkSet>, String>>;

struct FetchState {
    in_flight: Option<JwksFetchTx>,
    last_on_demand_refresh: Option<Instant>,
}

/// JWKS could not be fetched on the cold path: **503**.
#[derive(Debug, Clone)]
pub struct JwksUnavailable(pub String);

impl std::fmt::Display for JwksUnavailable {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.0)
    }
}

impl std::error::Error for JwksUnavailable {}

#[derive(Clone)]
pub struct JwtValidatorState {
    issuer: Arc<str>,
    jwks_uri: Arc<str>,
    allowed_audiences: Audiences,
    jwks: Arc<ArcSwapOption<JwkSet>>,
    client: reqwest::Client,
    /// In-flight fetch & unknown-kid refresh cooldown. Waiters subscribe; the
    /// network call runs without holding this lock.
    fetch_state: Arc<Mutex<FetchState>>,
    refresh_cooldown: Duration,
    refresh_started: Arc<AtomicBool>,
}

impl JwtValidatorState {
    pub fn new(issuer: String, jwks_uri: String, allowed_audiences: Audiences) -> Self {
        Self::with_refresh_cooldown(
            issuer,
            jwks_uri,
            allowed_audiences,
            Duration::from_secs(UNKNOWN_KID_REFRESH_COOLDOWN_SECS),
        )
    }

    pub fn with_refresh_cooldown(
        issuer: String,
        jwks_uri: String,
        allowed_audiences: Audiences,
        refresh_cooldown: Duration,
    ) -> Self {
        let client = reqwest::Client::builder()
            .timeout(Duration::from_secs(FETCH_TIMEOUT_SECS))
            .build()
            .expect("failed to create reqwest client");

        Self {
            issuer: issuer.into(),
            jwks_uri: jwks_uri.into(),
            allowed_audiences,
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

        let result = self
            .fetch_and_store()
            .await
            .map_err(|err| JwksUnavailable(err.to_string()));
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
        if result.is_ok() {
            self.try_start_refresh_task();
        }
        result
    }

    /// Triggers a singleflight-coalesced, cooldown-throttled JWKS refetch when
    /// a token presents a `kid` absent from the currently cached key set.
    ///
    /// Returns `Some(jwks)` if a refreshed key set is available (or if `kid`
    /// already appeared while waiting), or `None` if throttled or the refetch
    /// failed (leaving the existing cached JWKS intact).
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
                        let _ = tx.send(Err(err.to_string()));
                    }
                }
            }
        }
        match result {
            Ok(jwks) => {
                info!(
                    jwks_uri = %self.jwks_uri,
                    kid = %kid,
                    "jwks refreshed after unknown kid"
                );
                Some(jwks)
            }
            Err(err) => {
                warn!(
                    jwks_uri = %self.jwks_uri,
                    kid = %kid,
                    error_type = crate::error::ErrorKind::Network.as_str(),
                    error_message = %err,
                    "jwks on-demand refresh failed; keeping cached keys"
                );
                None
            }
        }
    }

    async fn fetch_and_store(&self) -> Result<Arc<JwkSet>, reqwest::Error> {
        let new_jwks = Arc::new(fetch_jwks(&self.client, &self.jwks_uri).await?);
        self.jwks.store(Some(Arc::clone(&new_jwks)));
        Ok(new_jwks)
    }

    pub fn get_issuer(&self) -> String {
        self.issuer.to_string()
    }

    pub fn get_allowed_audiences(&self) -> &Audiences {
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
                    error_type = crate::error::ErrorKind::Network.as_str(),
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
                                error_type = crate::error::ErrorKind::Network.as_str(),
                                error_message = %err,
                                "jwks background refresh failed"
                            );
                        } else {
                            warn!(
                                jwks_uri = %state.jwks_uri,
                                error_type = crate::error::ErrorKind::Network.as_str(),
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
    let res = client.get(uri).send().await?.error_for_status()?;
    res.json::<JwkSet>().await
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::AtomicUsize;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    use tokio::net::TcpListener;

    #[tokio::test]
    async fn cold_path_failure_does_not_thundering_herd() {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let hits = Arc::new(AtomicUsize::new(0));
        let counter = hits.clone();
        tokio::spawn(async move {
            loop {
                let Ok((mut sock, _)) = listener.accept().await else {
                    return;
                };
                counter.fetch_add(1, Ordering::SeqCst);
                tokio::spawn(async move {
                    let mut buf = [0u8; 1024];
                    let _ = sock.read(&mut buf).await;
                    tokio::time::sleep(Duration::from_millis(50)).await;
                    let _ = sock
                        .write_all(
                            b"HTTP/1.1 500 Internal Server Error\r\ncontent-length: 0\r\nconnection: close\r\n\r\n",
                        )
                        .await;
                });
            }
        });

        let state = JwtValidatorState::new(
            "https://auth".into(),
            format!("http://{addr}/jwks"),
            Audiences::Any,
        );
        let mut tasks = Vec::new();
        for _ in 0..8 {
            let s = state.clone();
            tasks.push(tokio::spawn(async move { s.get_jwks().await }));
        }
        for t in tasks {
            assert!(t.await.unwrap().is_err());
        }
        assert_eq!(
            hits.load(Ordering::SeqCst),
            1,
            "waiters must share the failed cold-path fetch instead of retrying concurrently"
        );
    }

    async fn spawn_dynamic_jwks(
        body: Arc<std::sync::Mutex<String>>,
        delay: Duration,
    ) -> (std::net::SocketAddr, Arc<AtomicUsize>) {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let hits = Arc::new(AtomicUsize::new(0));
        let counter = hits.clone();
        tokio::spawn(async move {
            loop {
                let Ok((mut sock, _)) = listener.accept().await else {
                    return;
                };
                counter.fetch_add(1, Ordering::SeqCst);
                let payload = body.lock().unwrap().clone();
                tokio::spawn(async move {
                    let mut buf = [0u8; 1024];
                    let _ = sock.read(&mut buf).await;
                    if !delay.is_zero() {
                        tokio::time::sleep(delay).await;
                    }
                    let resp = format!(
                        "HTTP/1.1 200 OK\r\ncontent-type: application/json\r\ncontent-length: {}\r\nconnection: close\r\n\r\n{}",
                        payload.len(),
                        payload
                    );
                    let _ = sock.write_all(resp.as_bytes()).await;
                });
            }
        });
        (addr, hits)
    }

    fn ed25519_jwks_json(kids: &[&str]) -> String {
        let keys: Vec<serde_json::Value> = kids
            .iter()
            .map(|kid| {
                serde_json::json!({
                    "kty": "OKP",
                    "crv": "Ed25519",
                    "kid": kid,
                    "use": "sig",
                    "alg": "EdDSA",
                    "x": "bXUj6dVFBJp4oH4PZBzpnTQAuK8oEtKDiftREsbKRFI"
                })
            })
            .collect();
        serde_json::json!({ "keys": keys }).to_string()
    }

    #[tokio::test]
    async fn unknown_kid_refetches_coalesces_and_respects_cooldown() {
        let body = Arc::new(std::sync::Mutex::new(ed25519_jwks_json(&["k1"])));
        let (addr, hits) = spawn_dynamic_jwks(body.clone(), Duration::from_millis(30)).await;

        let state = JwtValidatorState::with_refresh_cooldown(
            "https://auth".into(),
            format!("http://{addr}/jwks"),
            Audiences::Any,
            Duration::from_millis(150),
        );

        let initial = state.get_jwks().await.unwrap();
        assert!(initial.find("k1").is_some());
        assert!(initial.find("k2").is_none());
        assert_eq!(hits.load(Ordering::SeqCst), 1);

        // Rotate JWKS to add k2; 8 concurrent requests for k2 must share 1 refetch.
        *body.lock().unwrap() = ed25519_jwks_json(&["k1", "k2"]);
        let mut tasks = Vec::new();
        for _ in 0..8 {
            let s = state.clone();
            tasks.push(tokio::spawn(async move {
                s.refresh_for_unknown_kid("k2").await
            }));
        }
        for t in tasks {
            let jwks = t.await.unwrap().expect("expected refreshed jwks");
            assert!(jwks.find("k2").is_some());
        }
        assert_eq!(hits.load(Ordering::SeqCst), 2);

        // Another unknown kid within cooldown is throttled without hitting the IdP.
        assert!(state.refresh_for_unknown_kid("bogus").await.is_none());
        assert_eq!(hits.load(Ordering::SeqCst), 2);

        // After cooldown elapses, a new rotation triggers a refetch.
        tokio::time::sleep(Duration::from_millis(180)).await;
        *body.lock().unwrap() = ed25519_jwks_json(&["k2", "k3"]);
        let refreshed = state
            .refresh_for_unknown_kid("k3")
            .await
            .expect("expected refetch after cooldown");
        assert!(refreshed.find("k3").is_some());
        assert_eq!(hits.load(Ordering::SeqCst), 3);
    }
}
