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
use crate::pipeline::{path_and_query, Admitted, Forward};
use crate::route_map::Route;

use super::context::GatewayContext;

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

pub fn record_admission_span(ctx: &GatewayContext, admitted: &Admitted) {
    let (Some(span), Admitted::Caller(caller)) = (&ctx.root_span, admitted) else {
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
) -> std::result::Result<(), UpstreamError> {
    let req = session.req_header_mut();
    strip_client_credentials(req);
    strip_plat5_headers(req);
    for (name, value) in forward.headers {
        if let Err(err) = req.insert_header(name, value) {
            warn!(
                error_kind = ErrorKind::Internal.as_str(),
                error_message = %err,
                "failed to set a caller header"
            );
            return Err(UpstreamError::Internal);
        }
    }

    let query = session.req_header().uri.query().map(str::to_string);
    let path_and_query = path_and_query(&forward.path, query.as_deref());
    let new_uri = http::Uri::builder()
        .path_and_query(path_and_query.as_str())
        .build()
        .map_err(|err| {
            warn!(
                error_kind = ErrorKind::Internal.as_str(),
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

    let peer_addr = peer_address(&route.base_url);
    let Some(socket_addr) = resolve_peer(&peer_addr).await else {
        warn!(
            route = %route.path,
            upstream = %peer_addr,
            "upstream address is not a resolvable host:port"
        );
        return Err(UpstreamError::BadUpstream);
    };
    info!(route = %route.path, upstream = %peer_addr, "forwarding request to upstream");
    let mut peer = HttpPeer::new(socket_addr, false, String::new());
    peer.options.connection_timeout = Some(route.timeouts.connect);
    peer.options.read_timeout = Some(route.timeouts.read);
    ctx.upstream_peer = Some(Box::new(peer));
    Ok(())
}

/// Gateway peers are host:port. Strip accidental scheme/path from route config.
pub fn peer_address(base_url: &str) -> String {
    let s = base_url.trim();
    let without_scheme = s
        .strip_prefix("https://")
        .or_else(|| s.strip_prefix("http://"))
        .unwrap_or(s);
    without_scheme
        .split('/')
        .next()
        .unwrap_or(without_scheme)
        .to_string()
}

/// Resolve `host:port` without panicking and without blocking the executor
/// (`lookup_host` runs the lookup on the blocking pool). `None` when malformed
/// or unresolvable.
async fn resolve_peer(addr: &str) -> Option<std::net::SocketAddr> {
    tokio::net::lookup_host(addr).await.ok()?.next()
}

struct RequestHeaderInjector<'a> {
    headers: &'a mut RequestHeader,
}

impl Injector for RequestHeaderInjector<'_> {
    fn set(&mut self, key: &str, value: String) {
        let header_name = match HeaderName::from_bytes(key.as_bytes()) {
            Ok(name) => name,
            Err(err) => {
                warn!(key = %key, ?err, "failed to parse propagation header name");
                return;
            }
        };
        let header_value = match HeaderValue::from_str(&value) {
            Ok(val) => val,
            Err(err) => {
                warn!(key = %key, ?err, "failed to parse propagation header value");
                return;
            }
        };
        if let Err(err) = self.headers.insert_header(header_name, header_value) {
            warn!(key = %key, ?err, "failed to inject propagation header");
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn resolve_peer_rejects_bad_addresses() {
        assert!(resolve_peer("example.com").await.is_none());
        assert!(resolve_peer("").await.is_none());
        assert!(resolve_peer("host:notaport").await.is_none());
        assert_eq!(
            resolve_peer("127.0.0.1:3000").await.map(|a| a.port()),
            Some(3000)
        );
    }

    #[test]
    fn peer_address_strips_scheme_so_stored_bare_routes_still_work() {
        assert_eq!(peer_address("http://api:3000"), "api:3000");
        assert_eq!(peer_address("api:3000"), "api:3000");
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
}
