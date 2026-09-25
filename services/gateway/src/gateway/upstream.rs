use std::collections::HashMap;
use std::time::Duration;

use http::header::HeaderName;
use http::HeaderValue;
use opentelemetry::global;
use opentelemetry::propagation::Injector;
use pingora::http::RequestHeader;
use pingora::proxy::Session;
use pingora::upstreams::peer::HttpPeer;
use tracing::{debug, info, warn};
use tracing_opentelemetry::OpenTelemetrySpanExt;

use crate::admission::Admission;
use crate::error::ErrorKind;
use crate::route_map::Route;

use super::context::GatewayContext;
use super::rewrite::{self, RewriteError, SubjectRef};

const CLIENT_CREDENTIAL_HEADERS: &[&str] = &["Authorization", "X-API-Key"];

pub fn strip_client_credentials(req: &mut RequestHeader) {
    for name in CLIENT_CREDENTIAL_HEADERS {
        req.remove_header(*name);
    }
}

pub fn record_admission_span(ctx: &GatewayContext, admission: &Admission) {
    let Some(ref span) = ctx.root_span else {
        return;
    };
    match admission {
        Admission::Public => {}
        Admission::User { user_id, kid, .. } => {
            span.record("user.id", user_id.as_str());
            if let Some(kid) = kid {
                span.record("jwt.kid", kid.as_str());
            }
        }
        Admission::Organization {
            organization_id, ..
        } => {
            span.record("organization.id", organization_id.as_str());
        }
        Admission::Member {
            organization_id,
            member_id,
            ..
        } => {
            span.record("organization.id", organization_id.as_str());
            span.record("member.id", member_id.as_str());
        }
    }
}

/// Build the upstream peer for a matched route and store it in context.
pub fn build_and_store_upstream_peer(
    session: &mut Session,
    ctx: &mut GatewayContext,
    route: &Route,
    params: &HashMap<String, String>,
    admission: &Admission,
    connect_timeout: Duration,
    read_timeout: Duration,
) -> std::result::Result<(), RewriteError> {
    strip_client_credentials(session.req_header_mut());

    if let Some(template) = route.upstream.as_deref() {
        let upstream_path = rewrite::substitute(template, params, subject_ref(admission))?;
        let query = session.req_header().uri.query().map(str::to_string);
        let path_and_query = rewrite::path_and_query(&upstream_path, query.as_deref());
        let new_uri = http::Uri::builder()
            .path_and_query(path_and_query.as_str())
            .build()
            .map_err(|err| {
                warn!(
                    error_kind = ErrorKind::Internal.as_str(),
                    error_message = %err,
                    "failed to build upstream URI"
                );
                RewriteError::Internal
            })?;
        session.req_header_mut().set_uri(new_uri);
        debug!(
            route = %route.path,
            upstream = %upstream_path,
            "path rewritten"
        );
    }

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
    info!(route = %route.path, upstream = %peer_addr, "forwarding request to upstream");
    let mut peer = HttpPeer::new(peer_addr, false, String::new());
    peer.options.connection_timeout = Some(connect_timeout);
    peer.options.read_timeout = Some(read_timeout);
    ctx.upstream_peer = Some(Box::new(peer));
    Ok(())
}

fn subject_ref(admission: &Admission) -> SubjectRef<'_> {
    match admission {
        Admission::Public => SubjectRef {
            user_id: None,
            organization_id: None,
            member_id: None,
        },
        Admission::User { user_id, .. } => SubjectRef {
            user_id: Some(user_id.as_str()),
            organization_id: None,
            member_id: None,
        },
        Admission::Organization {
            organization_id, ..
        } => SubjectRef {
            user_id: None,
            organization_id: Some(organization_id.as_str()),
            member_id: None,
        },
        Admission::Member {
            organization_id,
            member_id,
            ..
        } => SubjectRef {
            user_id: None,
            organization_id: Some(organization_id.as_str()),
            member_id: Some(member_id.as_str()),
        },
    }
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

    #[test]
    fn strip_client_credentials_removes_auth_material() {
        let mut req = RequestHeader::build("GET", b"/api/x", None).unwrap();
        req.insert_header("Authorization", "Bearer secret-jwt")
            .unwrap();
        req.insert_header("X-API-Key", "plat5-sk-1-test").unwrap();
        req.insert_header("X-Request-ID", "req-1").unwrap();

        strip_client_credentials(&mut req);

        assert!(req.headers.get("Authorization").is_none());
        assert!(req.headers.get("X-API-Key").is_none());
        assert_eq!(
            req.headers
                .get("X-Request-ID")
                .and_then(|v| v.to_str().ok()),
            Some("req-1")
        );
    }

    #[test]
    fn strip_client_credentials_is_idempotent_when_absent() {
        let mut req = RequestHeader::build("GET", b"/public", None).unwrap();
        req.insert_header("X-Request-ID", "req-2").unwrap();
        strip_client_credentials(&mut req);
        assert!(req.headers.get("Authorization").is_none());
        assert!(req.headers.get("X-API-Key").is_none());
        assert_eq!(
            req.headers
                .get("X-Request-ID")
                .and_then(|v| v.to_str().ok()),
            Some("req-2")
        );
    }
}
