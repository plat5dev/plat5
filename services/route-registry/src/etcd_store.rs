use std::collections::HashMap;

use crate::route_config::{ServiceConfig, ROUTES_PREFIX};
use etcd_client::{Client, GetOptions};
use tracing::info;

#[derive(Clone)]
pub struct EtcdStore {
    client: Client,
}

#[derive(Debug)]
pub enum StoreError {
    Etcd(String),
    Parse(String),
}

impl std::fmt::Display for StoreError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            StoreError::Etcd(m) | StoreError::Parse(m) => write!(f, "{m}"),
        }
    }
}

impl std::error::Error for StoreError {}

impl EtcdStore {
    pub async fn connect(etcd_url: &str) -> Result<Self, StoreError> {
        let client = Client::connect([etcd_url], None)
            .await
            .map_err(|e| StoreError::Etcd(e.to_string()))?;
        Ok(Self { client })
    }

    pub async fn ping(&self) -> Result<(), StoreError> {
        self.client
            .kv_client()
            .get("health-check-nonexistent", None)
            .await
            .map_err(|e| StoreError::Etcd(e.to_string()))?;
        Ok(())
    }

    pub fn key(name: &str) -> String {
        format!("{ROUTES_PREFIX}{name}")
    }

    pub async fn put(&self, name: &str, config: &ServiceConfig) -> Result<(), StoreError> {
        let json = serde_json::to_string(config).map_err(|e| StoreError::Parse(e.to_string()))?;
        let key = Self::key(name);
        self.client
            .kv_client()
            .put(key.clone(), json, None)
            .await
            .map_err(|e| StoreError::Etcd(e.to_string()))?;
        info!(service = %name, key = %key, "route service upserted");
        Ok(())
    }

    pub async fn get(&self, name: &str) -> Result<Option<ServiceConfig>, StoreError> {
        let key = Self::key(name);
        let resp = self
            .client
            .kv_client()
            .get(key, None)
            .await
            .map_err(|e| StoreError::Etcd(e.to_string()))?;
        let Some(kv) = resp.kvs().first() else {
            return Ok(None);
        };
        let value = String::from_utf8_lossy(kv.value());
        let cfg: ServiceConfig =
            serde_json::from_str(&value).map_err(|e| StoreError::Parse(e.to_string()))?;
        Ok(Some(cfg))
    }

    pub async fn list(&self) -> Result<HashMap<String, ServiceConfig>, StoreError> {
        let resp = self
            .client
            .kv_client()
            .get(ROUTES_PREFIX, Some(GetOptions::new().with_prefix()))
            .await
            .map_err(|e| StoreError::Etcd(e.to_string()))?;

        let mut out = HashMap::new();
        for kv in resp.kvs() {
            let key = String::from_utf8_lossy(kv.key());
            let name = key.strip_prefix(ROUTES_PREFIX).unwrap_or("").to_string();
            if name.is_empty() {
                continue;
            }
            let value = String::from_utf8_lossy(kv.value());
            match serde_json::from_str::<ServiceConfig>(&value) {
                Ok(cfg) => {
                    out.insert(name, cfg);
                }
                Err(e) => {
                    return Err(StoreError::Parse(format!(
                        "invalid JSON for service '{name}': {e}"
                    )));
                }
            }
        }
        Ok(out)
    }

    pub async fn delete(&self, name: &str) -> Result<bool, StoreError> {
        let key = Self::key(name);
        let resp = self
            .client
            .kv_client()
            .delete(key, None)
            .await
            .map_err(|e| StoreError::Etcd(e.to_string()))?;
        Ok(resp.deleted() > 0)
    }
}
