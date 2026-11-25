//! Structured request and permission-denial logging (docs/reference/telemetry.md).

use opentelemetry::trace::TraceContextExt;
use tracing::{info, warn};
use tracing_opentelemetry::OpenTelemetrySpanExt;

use crate::pipeline::{Caller, RequestCredential};
use crate::route_map::{Required, Route};

use super::context::GatewayContext;

pub(super) fn otel_trace_ids(span: Option<&tracing::Span>) -> (Option<String>, Option<String>) {
    let Some(span) = span else {
        return (None, None);
    };
    let cx = span.context();
    let sc = cx.span().span_context().clone();
    if !sc.is_valid() {
        return (None, None);
    }
    (
        Some(sc.trace_id().to_string()),
        Some(sc.span_id().to_string()),
    )
}

pub(super) struct RequestLog<'a> {
    pub request_id: &'a str,
    pub route: &'a str,
    pub method: &'a str,
    pub status: u16,
    pub duration_ms: f64,
    pub trace_id: Option<&'a str>,
    pub span_id: Option<&'a str>,
    pub credential: &'a RequestCredential,
    /// Set on a 5xx only ([`request_error_type`]).
    pub error_type: Option<&'static str>,
}

/// `error_type` for a request that ended with `status`: on a 5xx only
/// (docs/reference/telemetry.md#error-fields-5xx-only), for the root span's
/// `error.type` and the request line. `network` when the proxy failed or on a 503,
/// `internal` on any other 5xx.
pub(super) fn request_error_type(status: u16, failed: bool) -> Option<&'static str> {
    match status {
        500..=599 if failed || status == 503 => Some("network"),
        500..=599 => Some("internal"),
        _ => None,
    }
}

/// `[a,b]`: a permission list as one log field.
fn permission_list(permissions: &[String]) -> String {
    format!("[{}]", permissions.join(","))
}

/// The why of a permission-denied 403, next to the request line
/// (docs/reference/telemetry.md#gateway-permission-denied-line).
pub(super) fn log_permission_denied(ctx: &GatewayContext, route: &Route, caller: &Caller) {
    let required = match &route.required {
        Required::AnyOf(list) => permission_list(list),
        Required::AnyMember => "any_member".to_string(),
        Required::Nothing => "[]".to_string(),
    };
    let cred = &ctx.credential;
    warn!(
        request_id = ctx.request_id.as_deref(),
        route = %route.path,
        method = %ctx.method_label(),
        status = 403u16,
        auth_type = cred.auth_type.map(|t| t.as_str()),
        key_id = cred.key_id.as_deref(),
        principal = cred.principal.as_deref(),
        root_id = cred.root_id.as_deref(),
        node_id = cred.node_id.as_deref(),
        member_id = cred.member_id.as_deref(),
        required_permissions = %required,
        permissions = %permission_list(&caller.permissions),
        "required_permissions denied"
    );
}

/// One line per request (docs/reference/telemetry.md#gateway-request-line). Credential
/// fields are present only when known: `key_id` once admit recognized a key, the ids
/// once admitted. Never the key or token itself, never a principal id.
pub(super) fn log_request_outcome(log: RequestLog<'_>, error: Option<&pingora::Error>) {
    let RequestLog {
        request_id,
        route,
        method,
        status,
        duration_ms,
        trace_id,
        span_id,
        credential,
        error_type,
    } = log;
    let auth_type = credential.auth_type.map(|t| t.as_str());
    let key_id = credential.key_id.as_deref();
    let principal = credential.principal.as_deref();
    let root_id = credential.root_id.as_deref();
    let node_id = credential.node_id.as_deref();
    let member_id = credential.member_id.as_deref();
    let user_id = credential.user_id.as_deref();

    match error {
        Some(err) => {
            warn!(
                trace_id,
                span_id,
                request_id,
                route = %route,
                method = %method,
                status,
                duration_ms,
                auth_type,
                key_id,
                principal,
                root_id,
                node_id,
                member_id,
                user_id,
                error_type,
                error_message = error_type.map(|_| tracing::field::display(err)),
                "request failed"
            );
        }
        None => {
            info!(
                trace_id,
                span_id,
                request_id,
                route = %route,
                method = %method,
                status,
                duration_ms,
                auth_type,
                key_id,
                principal,
                root_id,
                node_id,
                member_id,
                user_id,
                error_type,
                "request completed"
            );
        }
    }
}

#[cfg(test)]
mod tests {
    use pingora::ErrorType;

    use super::*;

    fn request_line(status: u16, error: Option<&pingora::Error>) -> serde_json::Value {
        let credential = RequestCredential::default();
        let mut lines = crate::telemetry::logged(|| {
            log_request_outcome(
                RequestLog {
                    request_id: "req-1",
                    route: "/orgs/{organization}",
                    method: "GET",
                    status,
                    duration_ms: 2.5,
                    trace_id: None,
                    span_id: None,
                    credential: &credential,
                    error_type: request_error_type(status, error.is_some()),
                },
                error,
            )
        });
        assert_eq!(lines.len(), 1, "{lines:?}");
        lines.remove(0)
    }

    #[test]
    fn request_line_error_fields_on_5xx_only() {
        let refused = pingora::Error::new(ErrorType::ConnectRefused);
        let line = request_line(502, Some(&refused));
        assert_eq!(line["message"], "request failed");
        assert_eq!(line["level"], "warn");
        assert_eq!(line["error_type"], "network");
        assert!(
            line["error_message"]
                .as_str()
                .unwrap()
                .contains("ConnectRefused"),
            "{line}"
        );

        let line = request_line(500, None);
        assert_eq!(line["message"], "request completed");
        assert_eq!(line["level"], "info");
        assert_eq!(line["error_type"], "internal");
        assert!(line.get("error_message").is_none(), "{line}");
        assert_eq!(request_line(503, None)["error_type"], "network");

        let closed = pingora::Error::new(ErrorType::ReadError);
        for (status, error) in [
            (400, Some(&*closed)),
            (413, Some(&*closed)),
            (0, Some(&*closed)),
            (401, None),
            (200, None),
        ] {
            let line = request_line(status, error);
            assert!(line.get("error_type").is_none(), "{status}: {line}");
            assert!(line.get("error_message").is_none(), "{status}: {line}");
            assert!(line.get("error").is_none(), "{status}: {line}");
        }
    }
}
