//! Shared SSRF IP filtering and short-lived cached DNS resolution used by
//! both upstream proxying (`gateway::upstream`) and outbound AuthZEN PDPs
//! (`authorizer::client`).

use std::future::Future;
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr, SocketAddr};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Arc;
use std::time::Duration;

use moka::future::Cache;
use tracing::warn;

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

/// Short-lived cached DNS resolver keyed by hostname, with round-robin selection
/// and optional private/reserved IP blocking. Failed or blocked lookups are never
/// cached, and concurrent misses for the same host share one lookup.
#[derive(Clone)]
pub struct PeerResolver {
    cache: Cache<String, Arc<ResolvedHost>>,
    block_private_ips: bool,
}

impl PeerResolver {
    pub fn new(block_private_ips: bool) -> Self {
        Self::with_ttl(block_private_ips, DNS_CACHE_TTL)
    }

    pub fn with_ttl(block_private_ips: bool, ttl: Duration) -> Self {
        let cache = Cache::builder()
            .max_capacity(DNS_CACHE_CAPACITY)
            .time_to_live(ttl)
            .build();
        Self {
            cache,
            block_private_ips,
        }
    }

    pub fn block_private_ips(&self) -> bool {
        self.block_private_ips
    }

    pub async fn resolve(&self, addr: &str) -> Option<SocketAddr> {
        self.resolve_with(addr, lookup_host_addrs).await
    }

    /// Resolves all allowed addresses for `(host, port)` (used by `reqwest::dns::Resolve`
    /// so HTTP clients connect directly to the SSRF-checked IPs without TOCTOU re-resolution).
    pub async fn resolve_all(&self, host: &str, port: u16) -> Result<Vec<SocketAddr>, String> {
        let host = host.trim_start_matches('[').trim_end_matches(']');
        if let Ok(ip) = host.parse::<IpAddr>() {
            if self.block_private_ips && is_blocked_ip(ip) {
                warn!(host = %host, ip = %ip, "blocked outbound connection to private or reserved IP");
                return Err(format!("blocked private or reserved IP {ip}"));
            }
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
            if self.block_private_ips && is_blocked_ip(sa.ip()) {
                warn!(
                    upstream = %addr,
                    ip = %sa.ip(),
                    "blocked upstream connection to private or reserved IP"
                );
                return None;
            }
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
        let block_private_ips = self.block_private_ips;
        self.cache
            .try_get_with(key, async move {
                let addrs = lookup(query_addr.clone())
                    .await
                    .filter(|v| !v.is_empty())
                    .ok_or_else(|| format!("dns lookup failed for {query_addr}"))?;
                if block_private_ips {
                    for resolved in &addrs {
                        if is_blocked_ip(resolved.ip()) {
                            warn!(
                                upstream = %query_addr,
                                ip = %resolved.ip(),
                                "blocked outbound connection to private or reserved IP"
                            );
                            return Err(format!(
                                "blocked private or reserved IP {}",
                                resolved.ip()
                            ));
                        }
                    }
                }
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
            .map_err(|e| (*e).clone())
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

/// True for loopback, RFC1918 private, link-local (including cloud metadata),
/// CGNAT (`100.64.0.0/10`), `0.0.0.0/8`, IPv6 ULA (`fc00::/7`), unspecified,
/// broadcast, multicast, or an IPv4-mapped IPv6 address pointing to any of those.
pub fn is_blocked_ip(ip: IpAddr) -> bool {
    match ip {
        IpAddr::V4(v4) => is_blocked_ipv4(v4),
        IpAddr::V6(v6) => is_blocked_ipv6(v6),
    }
}

fn is_blocked_ipv4(ip: Ipv4Addr) -> bool {
    let o = ip.octets();
    ip.is_loopback()
        || ip.is_private()
        || ip.is_link_local()
        || ip.is_unspecified()
        || ip.is_broadcast()
        || ip.is_multicast()
        || o[0] == 0 // 0.0.0.0/8 ("this network")
        || (o[0] == 100 && (o[1] & 0xc0) == 64) // 100.64.0.0/10 (CGNAT / shared address space)
}

fn is_blocked_ipv6(ip: Ipv6Addr) -> bool {
    if let Some(v4) = ip.to_ipv4_mapped() {
        return is_blocked_ipv4(v4);
    }
    let s = ip.segments();
    ip.is_loopback()
        || ip.is_unspecified()
        || ip.is_multicast()
        || (s[0] & 0xfe00) == 0xfc00 // fc00::/7 (ULA)
        || (s[0] & 0xffc0) == 0xfe80 // fe80::/10 (link-local)
}
