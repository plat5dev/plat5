//! Short-lived cached DNS resolution used by both upstream proxying
//! (`gateway::upstream`) and outbound AuthZEN PDPs (`authorizer::client`).

use std::future::Future;
use std::net::{IpAddr, SocketAddr};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Arc;
use std::time::Duration;

use moka::future::Cache;

const DNS_CACHE_TTL: Duration = Duration::from_secs(10);
const DNS_CACHE_CAPACITY: u64 = 1_024;

struct ResolvedHost {
    ips: Vec<IpAddr>,
    next: AtomicUsize,
}

impl ResolvedHost {
    fn pick(&self) -> IpAddr {
        let idx = self.next.fetch_add(1, Ordering::Relaxed) % self.ips.len();
        self.ips[idx]
    }
}

/// Short-lived cached DNS resolver keyed by hostname, with round-robin selection.
/// Failed lookups are never cached, and concurrent misses for the same host share one lookup.
#[derive(Clone)]
pub struct PeerResolver {
    cache: Cache<String, Arc<ResolvedHost>>,
}

impl Default for PeerResolver {
    fn default() -> Self {
        Self::new()
    }
}

impl PeerResolver {
    pub fn new() -> Self {
        Self::with_ttl(DNS_CACHE_TTL)
    }

    pub fn with_ttl(ttl: Duration) -> Self {
        let cache = Cache::builder()
            .max_capacity(DNS_CACHE_CAPACITY)
            .time_to_live(ttl)
            .build();
        Self { cache }
    }

    pub async fn resolve(&self, addr: &str) -> Option<SocketAddr> {
        self.resolve_with(addr, lookup_host_addrs).await
    }

    /// Resolves all addresses for `(host, port)` (used by `reqwest::dns::Resolve`).
    pub async fn resolve_all(&self, host: &str, port: u16) -> Result<Vec<SocketAddr>, String> {
        let host = host.trim_start_matches('[').trim_end_matches(']');
        if let Ok(ip) = host.parse::<IpAddr>() {
            return Ok(vec![SocketAddr::new(ip, port)]);
        }
        let peer = self
            .resolve_host_with(host, port, lookup_host_addrs)
            .await?;
        Ok(peer
            .ips
            .iter()
            .map(|ip| SocketAddr::new(*ip, port))
            .collect())
    }

    pub async fn resolve_with<F, Fut>(&self, addr: &str, lookup: F) -> Option<SocketAddr>
    where
        F: FnOnce(String) -> Fut,
        Fut: Future<Output = Option<Vec<SocketAddr>>>,
    {
        if let Ok(sa) = addr.parse::<SocketAddr>() {
            return Some(sa);
        }
        let (host, port) = split_host_port(addr)?;
        let peer = self.resolve_host_with(host, port, lookup).await.ok()?;
        Some(SocketAddr::new(peer.pick(), port))
    }

    async fn resolve_host_with<F, Fut>(
        &self,
        host: &str,
        port: u16,
        lookup: F,
    ) -> Result<Arc<ResolvedHost>, String>
    where
        F: FnOnce(String) -> Fut,
        Fut: Future<Output = Option<Vec<SocketAddr>>>,
    {
        let key = host.to_ascii_lowercase();
        let query_addr = format!("{host}:{port}");
        self.cache
            .try_get_with(key, async move {
                let addrs = lookup(query_addr.clone())
                    .await
                    .filter(|v| !v.is_empty())
                    .ok_or_else(|| format!("dns lookup failed for {query_addr}"))?;
                let prefer_ipv4 = addrs[0].is_ipv4();
                let ips: Vec<IpAddr> = addrs
                    .into_iter()
                    .filter(|sa| sa.is_ipv4() == prefer_ipv4)
                    .map(|sa| sa.ip())
                    .collect();
                Ok(Arc::new(ResolvedHost {
                    ips,
                    next: AtomicUsize::new(0),
                }))
            })
            .await
            .map_err(|e: Arc<String>| (*e).clone())
    }
}

fn split_host_port(addr: &str) -> Option<(&str, u16)> {
    let (host, port_str) = if let Some(rest) = addr.strip_prefix('[') {
        let (h, after) = rest.split_once(']')?;
        let p = after.strip_prefix(':')?;
        (h, p)
    } else {
        addr.rsplit_once(':')?
    };
    if host.is_empty() {
        return None;
    }
    let port = port_str.parse::<u16>().ok()?;
    (port > 0).then_some((host, port))
}

impl reqwest::dns::Resolve for PeerResolver {
    fn resolve(&self, name: reqwest::dns::Name) -> reqwest::dns::Resolving {
        let resolver = self.clone();
        let host = name.as_str().to_string();
        Box::pin(async move {
            let addrs = resolver
                .resolve_all(&host, 0)
                .await
                .map_err(|e| -> Box<dyn std::error::Error + Send + Sync> { e.into() })?;
            let iter: reqwest::dns::Addrs = Box::new(addrs.into_iter());
            Ok(iter)
        })
    }
}

async fn lookup_host_addrs(addr: String) -> Option<Vec<SocketAddr>> {
    let addrs: Vec<SocketAddr> = tokio::net::lookup_host(addr).await.ok()?.collect();
    (!addrs.is_empty()).then_some(addrs)
}
