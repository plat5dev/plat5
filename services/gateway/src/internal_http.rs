use std::time::Duration;

use serde::de::DeserializeOwned;
use serde::Serialize;

const INTERNAL_TOKEN_HEADER: &str = "X-Plat5-Internal-Token";
const DEFAULT_TIMEOUT_SECS: u64 = 5;

/// Shared HTTP client for gateway → identity (and similar) internal calls.
/// Domain modules keep their own request/response types; this only does transport.
#[derive(Clone)]
pub struct InternalHttpClient {
    client: reqwest::Client,
    internal_token: Option<String>,
}

#[derive(Debug)]
pub enum InternalHttpError {
    Network(String),
    /// Non-success HTTP status (body not decoded as domain type).
    HttpStatus {
        status: u16,
    },
    Decode(String),
}

impl InternalHttpError {
    pub fn is_not_found(&self) -> bool {
        matches!(self, InternalHttpError::HttpStatus { status: 404 })
    }
}

impl InternalHttpClient {
    pub fn new(internal_token: Option<String>) -> Self {
        Self::with_timeout(Duration::from_secs(DEFAULT_TIMEOUT_SECS), internal_token)
    }

    pub fn with_timeout(timeout: Duration, internal_token: Option<String>) -> Self {
        let client = reqwest::Client::builder()
            .timeout(timeout)
            .build()
            .expect("failed to create reqwest client");

        Self {
            client,
            internal_token,
        }
    }

    /// POST JSON. On 2xx, deserialize body as `R`. Otherwise `HttpStatus` (no body parse).
    pub async fn post_json<B, R>(&self, url: &str, body: &B) -> Result<R, InternalHttpError>
    where
        B: Serialize + ?Sized,
        R: DeserializeOwned,
    {
        let mut req = self.client.post(url).json(body);
        if let Some(token) = &self.internal_token {
            req = req.header(INTERNAL_TOKEN_HEADER, token);
        }

        let response = req
            .send()
            .await
            .map_err(|e| InternalHttpError::Network(e.to_string()))?;

        let status = response.status();
        if !status.is_success() {
            return Err(InternalHttpError::HttpStatus {
                status: status.as_u16(),
            });
        }

        response
            .json::<R>()
            .await
            .map_err(|e| InternalHttpError::Decode(e.to_string()))
    }
}
