use http::header::HeaderName;
use http::HeaderValue;
use opentelemetry::global;
use opentelemetry::propagation::Injector;
use pingora::http::RequestHeader;
use pingora::proxy::Session;
use pingora::upstreams::peer::HttpPeer;
use tracing::{debug, info, warn};
use tracing_opentelemetry::OpenTelemetrySpanExt;

use crate::error::ErrorKind;
pub use crate::net::PeerResolver;
use crate::pipeline::{Caller, Forward};
use crate::route_map::Route;

use super::context::GatewayContext;
use super::signing::{UpstreamSigner, SIGNATURE_HEADER};

/// Consumed by the gateway; never leaked to services.
pub fn strip_client_credentials(req: &mut RequestHeader) {
    req.remove_header("Authorization");
}

/// Every `X-Plat5-*` a client sent: a client can't name a node or a member
/// (docs/reference/headers.md#stripped).
pub fn strip_plat5_headers(req: &mut RequestHeader) {
    let names: Vec<HeaderName> = req
        .headers
        .keys()
        .filter(|name| name.as_str().starts_with("x-plat5-"))
        .cloned()
        .collect();
    for name in names {
        req.remove_header(&name);
    }
}

/// The admitted caller on the request span, forwarded or refused after admit.
pub fn record_admission_span(ctx: &GatewayContext, caller: &Caller) {
    let Some(span) = &ctx.root_span else {
        return;
    };
    if let Some(member) = &caller.member_id {
        span.record("plat5.member_id", member.as_str());
    }
    if let Some(user) = &caller.user_id {
        span.record("plat5.user_id", user.as_str());
    }
    if let Some(root) = caller.root_id() {
        span.record("plat5.root_id", root);
    }
    if let Some(node) = caller.node_id() {
        span.record("plat5.node_id", node);
    }
    if let Some(kid) = &caller.kid {
        span.record("jwt.kid", kid.as_str());
    }
}

#[derive(Debug)]
pub enum UpstreamError {
    /// The rewritten path didn't make a URI. The load check should have refused it.
    Internal,
    /// Route base URL is not a usable `host:port`.
    BadUpstream,
}

/// Rewrite the request for the service and store the upstream peer in context.
pub async fn build_and_store_upstream_peer(
    session: &mut Session,
    ctx: &mut GatewayContext,
    route: &Route,
    forward: Forward,
    request_id: &str,
    signer: Option<&UpstreamSigner>,
    resolver: &PeerResolver,
) -> std::result::Result<(), UpstreamError> {
    let query = session.req_header().uri.query().map(str::to_string);
    let path_and_query = path_and_query(&forward.path, query.as_deref());
    let signature = signer.map(|s| {
        s.sign_now(
            session.req_header().method.as_str(),
            &path_and_query,
            request_id,
            forward
                .headers
                .iter()
                .map(|(k, v)| (k.as_str(), v.as_str())),
        )
    });

    let req = session.req_header_mut();
    strip_client_credentials(req);
    strip_plat5_headers(req);
    for (name, value) in forward.headers {
        if let Err(err) = req.insert_header(name, value) {
            warn!(
                error_type = ErrorKind::Internal.as_str(),
                error_message = %err,
                "failed to set a caller header"
            );
            return Err(UpstreamError::Internal);
        }
    }
    if let Some(sig) = signature {
        if let Err(err) = req.insert_header(SIGNATURE_HEADER, sig) {
            warn!(
                error_type = ErrorKind::Internal.as_str(),
                error_message = %err,
                "failed to set upstream signature header"
            );
            return Err(UpstreamError::Internal);
        }
    }

    let new_uri = http::Uri::builder()
        .path_and_query(path_and_query.as_str())
        .build()
        .map_err(|err| {
            warn!(
                error_type = ErrorKind::Internal.as_str(),
                error_message = %err,
                "failed to build upstream URI"
            );
            UpstreamError::Internal
        })?;
    session.req_header_mut().set_uri(new_uri);
    debug!(route = %route.path, upstream = %forward.path, "path rewritten");

    {
        let mut injector = RequestHeaderInjector {
            headers: session.req_header_mut(),
        };
        let cx = ctx
            .root_span()
            .map(|span| span.context())
            .unwrap_or_else(|| tracing::Span::current().context());
        global::get_text_map_propagator(|propagator| propagator.inject_context(&cx, &mut injector));
    }

    let Some(upstream) = parse_upstream(&route.base_url) else {
        warn!(
            route = %route.path,
            upstream = %route.base_url,
            "upstream address is not a valid host[:port]"
        );
        return Err(UpstreamError::BadUpstream);
    };
    let peer_addr = upstream.addr();
    let Some(socket_addr) = resolver.resolve(&peer_addr).await else {
        warn!(
            route = %route.path,
            upstream = %peer_addr,
            "upstream address is not a resolvable or allowed host:port"
        );
        return Err(UpstreamError::BadUpstream);
    };
    if upstream.tls {
        if let Err(err) = session
            .req_header_mut()
            .insert_header("Host", upstream.host_header())
        {
            warn!(
                error_type = ErrorKind::Internal.as_str(),
                error_message = %err,
                "failed to set upstream Host header"
            );
            return Err(UpstreamError::Internal);
        }
    }
    info!(route = %route.path, upstream = %peer_addr, tls = upstream.tls, "forwarding request to upstream");
    let sni = if upstream.tls {
        upstream.host.clone()
    } else {
        String::new()
    };
    let mut peer = HttpPeer::new(socket_addr, upstream.tls, sni);
    peer.options.connection_timeout = Some(route.timeouts.connect);
    peer.options.read_timeout = Some(route.timeouts.read);
    ctx.upstream_peer = Some(Box::new(peer));
    Ok(())
}

fn path_and_query(path: &str, query: Option<&str>) -> String {
    match query {
        Some(q) if !q.is_empty() => format!("{path}?{q}"),
        _ => path.to_string(),
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ParsedUpstream {
    pub tls: bool,
    pub host: String,
    pub port: u16,
}

impl ParsedUpstream {
    pub fn addr(&self) -> String {
        format!("{}:{}", self.host, self.port)
    }

    pub fn host_header(&self) -> String {
        let default_port = if self.tls { 443 } else { 80 };
        if self.port == default_port {
            self.host.clone()
        } else {
            format!("{}:{}", self.host, self.port)
        }
    }
}

/// Parse `http://host[:port]` (default 80), `https://host[:port]` (default 443),
/// or legacy bare `host:port`.
pub fn parse_upstream(base_url: &str) -> Option<ParsedUpstream> {
    let s = base_url.trim();
    let (tls, default_port, rest) = if let Some(rest) = s.strip_prefix("https://") {
        (true, Some(443u16), rest)
    } else if let Some(rest) = s.strip_prefix("http://") {
        (false, Some(80u16), rest)
    } else {
        (false, None, s)
    };
    let authority = rest.split('/').next().unwrap_or(rest);
    if authority.is_empty() {
        return None;
    }
    let (host, port) = match authority.rsplit_once(':') {
        Some((h, p)) => {
            let port = p.parse::<u16>().ok()?;
            if port == 0 {
                return None;
            }
            (h, port)
        }
        None => (authority, default_port?),
    };
    if host.is_empty() {
        return None;
    }
    Some(ParsedUpstream {
        tls,
        host: host.to_string(),
        port,
    })
}

#[cfg(test)]
pub fn peer_address(base_url: &str) -> String {
    parse_upstream(base_url)
        .map(|u| u.addr())
        .unwrap_or_else(|| base_url.trim().to_string())
}

struct RequestHeaderInjector<'a> {
    headers: &'a mut RequestHeader,
}

impl Injector for RequestHeaderInjector<'_> {
    fn set(&mut self, key: &str, value: String) {
        let header_name = match HeaderName::from_bytes(key.as_bytes()) {
            Ok(name) => name,
            Err(err) => {
                warn!(key = %key, error = ?err, "failed to parse propagation header name");
                return;
            }
        };
        let header_value = match HeaderValue::from_str(&value) {
            Ok(val) => val,
            Err(err) => {
                warn!(key = %key, error = ?err, "failed to parse propagation header value");
                return;
            }
        };
        if let Err(err) = self.headers.insert_header(header_name, header_value) {
            warn!(key = %key, error = ?err, "failed to inject propagation header");
        }
    }
}

#[cfg(test)]
mod tests {
    use std::net::{IpAddr, SocketAddr};
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::sync::Arc;
    use std::time::Duration;

    use super::*;
    use crate::net::is_blocked_ip;

    #[tokio::test]
    async fn resolve_peer_rejects_bad_addresses() {
        let open = PeerResolver::new(false);
        let strict = PeerResolver::new(true);
        assert!(open.resolve("example.com").await.is_none());
        assert!(open.resolve("").await.is_none());
        assert!(open.resolve("host:notaport").await.is_none());
        assert_eq!(
            open.resolve("127.0.0.1:3000").await.map(|a| a.port()),
            Some(3000)
        );
        assert!(strict.resolve("127.0.0.1:3000").await.is_none());
        assert!(strict.resolve("169.254.169.254:80").await.is_none());
        assert_eq!(
            strict.resolve("93.184.216.34:443").await.map(|a| a.port()),
            Some(443)
        );
    }

    #[tokio::test]
    async fn peer_resolver_caches_and_round_robins_records() {
        let resolver = PeerResolver::with_ttl(true, Duration::from_millis(80));
        let calls = Arc::new(AtomicUsize::new(0));
        let a: SocketAddr = "93.184.216.34:443".parse().unwrap();
        let b: SocketAddr = "93.184.216.35:443".parse().unwrap();

        let lookup = |calls: Arc<AtomicUsize>, res: Option<Vec<SocketAddr>>| {
            move |_addr: String| async move {
                calls.fetch_add(1, Ordering::SeqCst);
                res
            }
        };

        assert_eq!(
            resolver
                .resolve_with(
                    "api.example.com:443",
                    lookup(calls.clone(), Some(vec![a, b]))
                )
                .await,
            Some(a)
        );
        assert_eq!(
            resolver
                .resolve_with(
                    "api.example.com:443",
                    lookup(calls.clone(), Some(vec![a, b]))
                )
                .await,
            Some(b)
        );
        assert_eq!(
            resolver
                .resolve_with(
                    "api.example.com:443",
                    lookup(calls.clone(), Some(vec![a, b]))
                )
                .await,
            Some(a)
        );
        assert_eq!(
            calls.load(Ordering::SeqCst),
            1,
            "subsequent calls hit cache"
        );

        // After TTL expires, the resolver looks up again.
        tokio::time::sleep(Duration::from_millis(120)).await;
        assert_eq!(
            resolver
                .resolve_with("api.example.com:443", lookup(calls.clone(), Some(vec![b])))
                .await,
            Some(b)
        );
        assert_eq!(calls.load(Ordering::SeqCst), 2);
    }

    #[tokio::test]
    async fn peer_resolver_does_not_cache_blocked_or_failed_lookups() {
        let resolver = PeerResolver::with_ttl(true, Duration::from_secs(60));
        let calls = Arc::new(AtomicUsize::new(0));
        let private: SocketAddr = "10.0.0.5:8080".parse().unwrap();
        let public: SocketAddr = "93.184.216.34:8080".parse().unwrap();

        let run = |res: Option<Vec<SocketAddr>>| {
            let calls = calls.clone();
            move |_addr: String| async move {
                calls.fetch_add(1, Ordering::SeqCst);
                res
            }
        };

        assert!(resolver
            .resolve_with("flaky.example.com:8080", run(None))
            .await
            .is_none());
        assert!(resolver
            .resolve_with("flaky.example.com:8080", run(Some(vec![public, private])))
            .await
            .is_none());
        assert_eq!(
            resolver
                .resolve_with("flaky.example.com:8080", run(Some(vec![public])))
                .await,
            Some(public)
        );
        assert_eq!(calls.load(Ordering::SeqCst), 3);
    }

    #[tokio::test]
    async fn peer_resolver_sticks_to_preferred_address_family() {
        let resolver = PeerResolver::with_ttl(true, Duration::from_secs(60));
        let v4_a: SocketAddr = "93.184.216.34:443".parse().unwrap();
        let v4_b: SocketAddr = "93.184.216.35:443".parse().unwrap();
        let v6_a: SocketAddr = "[2606:2800:220:1:248:1893:25c8:1946]:443".parse().unwrap();
        let v6_b: SocketAddr = "[2606:2800:220:1:248:1893:25c8:1947]:443".parse().unwrap();
        let v6_private: SocketAddr = "[::1]:443".parse().unwrap();

        let give = |res: Vec<SocketAddr>| move |_addr: String| async move { Some(res) };

        // IPv4 first: round-robins across IPv4 records only.
        for expected in [v4_a, v4_b, v4_a] {
            assert_eq!(
                resolver
                    .resolve_with("v4first.example.com:443", give(vec![v4_a, v6_a, v4_b]))
                    .await,
                Some(expected)
            );
        }

        // IPv6 first: round-robins across IPv6 records only.
        for expected in [v6_a, v6_b, v6_a] {
            assert_eq!(
                resolver
                    .resolve_with("v6first.example.com:443", give(vec![v6_a, v4_a, v6_b]))
                    .await,
                Some(expected)
            );
        }

        // A private IP in the non-preferred family still fails SSRF checks.
        assert!(resolver
            .resolve_with("mixed.example.com:443", give(vec![v4_a, v6_private]))
            .await
            .is_none());
    }

    #[test]
    fn blocked_ips_cover_private_metadata_cgnat_and_mapped_v6() {
        for blocked in [
            "127.0.0.1",
            "127.255.255.254",
            "10.0.0.1",
            "172.16.0.1",
            "172.31.255.254",
            "192.168.1.1",
            "169.254.169.254",
            "100.64.0.1",
            "100.127.255.254",
            "0.0.0.0",
            "0.1.2.3",
            "224.0.0.1",
            "255.255.255.255",
            "::1",
            "::",
            "fc00::1",
            "fd12:3456::1",
            "fe80::1",
            "ff02::1",
            "::ffff:127.0.0.1",
            "::ffff:169.254.169.254",
            "::ffff:10.0.0.5",
            "::ffff:100.64.1.1",
        ] {
            let ip: IpAddr = blocked.parse().unwrap();
            assert!(is_blocked_ip(ip), "expected {blocked} to be blocked");
        }

        for allowed in [
            "1.1.1.1",
            "8.8.8.8",
            "93.184.216.34",
            "100.63.255.255",
            "100.128.0.1",
            "172.32.0.1",
            "2606:4700:4700::1111",
            "::ffff:93.184.216.34",
        ] {
            let ip: IpAddr = allowed.parse().unwrap();
            assert!(!is_blocked_ip(ip), "expected {allowed} to be allowed");
        }
    }

    #[test]
    fn peer_address_strips_scheme_so_stored_bare_routes_still_work() {
        assert_eq!(peer_address("http://api:3000"), "api:3000");
        assert_eq!(peer_address("api:3000"), "api:3000");
        assert_eq!(peer_address("http://api"), "api:80");
        assert_eq!(
            peer_address("https://api.example.com"),
            "api.example.com:443"
        );
        assert_eq!(
            peer_address("https://api.example.com:8443"),
            "api.example.com:8443"
        );
    }

    #[test]
    fn parse_upstream_handles_http_https_and_default_ports() {
        let http_default = parse_upstream("http://api.internal").unwrap();
        assert_eq!(
            http_default,
            ParsedUpstream {
                tls: false,
                host: "api.internal".into(),
                port: 80
            }
        );
        assert_eq!(http_default.host_header(), "api.internal");

        let https_default = parse_upstream("https://svc.run.app").unwrap();
        assert_eq!(
            https_default,
            ParsedUpstream {
                tls: true,
                host: "svc.run.app".into(),
                port: 443
            }
        );
        assert_eq!(https_default.addr(), "svc.run.app:443");
        assert_eq!(https_default.host_header(), "svc.run.app");

        let https_custom = parse_upstream("https://svc.run.app:8443").unwrap();
        assert_eq!(https_custom.addr(), "svc.run.app:8443");
        assert_eq!(https_custom.host_header(), "svc.run.app:8443");

        assert!(parse_upstream("").is_none());
        assert!(parse_upstream("http://api:0").is_none());
        assert!(parse_upstream("barehost").is_none());
    }

    #[test]
    fn strip_client_credentials_removes_auth_material() {
        let mut req = RequestHeader::build("GET", b"/api/x", None).unwrap();
        req.insert_header("Authorization", "Bearer secret-jwt")
            .unwrap();
        req.insert_header("X-Request-ID", "req-1").unwrap();

        strip_client_credentials(&mut req);

        assert!(req.headers.get("Authorization").is_none());
        assert_eq!(
            req.headers
                .get("X-Request-ID")
                .and_then(|v| v.to_str().ok()),
            Some("req-1")
        );
    }

    #[test]
    fn strips_every_plat5_header_a_client_sent() {
        let mut req = RequestHeader::build("GET", b"/x", None).unwrap();
        req.insert_header("X-Plat5-Member-Id", "01forged").unwrap();
        req.insert_header("X-Plat5-User-Id", "01forged").unwrap();
        req.insert_header("x-plat5-project-id", "01forged").unwrap();
        req.insert_header("X-Plat5-Audit-Details", "{}").unwrap();
        req.insert_header("X-Request-ID", "req-1").unwrap();
        strip_plat5_headers(&mut req);
        assert_eq!(req.headers.len(), 1);
        assert!(req.headers.get("X-Request-ID").is_some());
    }

    #[test]
    fn strip_client_credentials_is_idempotent_when_absent() {
        let mut req = RequestHeader::build("GET", b"/public", None).unwrap();
        req.insert_header("X-Request-ID", "req-2").unwrap();
        strip_client_credentials(&mut req);
        assert!(req.headers.get("Authorization").is_none());
        assert_eq!(
            req.headers
                .get("X-Request-ID")
                .and_then(|v| v.to_str().ok()),
            Some("req-2")
        );
    }

    #[test]
    fn preserves_query() {
        assert_eq!(
            path_and_query("/users/01u/nodes", Some("kind=project")),
            "/users/01u/nodes?kind=project"
        );
        assert_eq!(path_and_query("/users/01u", None), "/users/01u");
    }
}
