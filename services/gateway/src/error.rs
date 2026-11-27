use serde::Serialize;
use std::fmt;

use crate::route_map::{Required, Scope};

/// The 401 message where any credential is taken.
const ANY_CREDENTIAL: &str = "Authentication required: a sign-in token or an API key.";

/// Error kinds for 5xx telemetry (span attributes and log fields).
#[derive(Debug, Clone, Copy)]
pub enum ErrorKind {
    Network,
    Internal,
}

impl ErrorKind {
    pub fn as_str(&self) -> &'static str {
        match self {
            ErrorKind::Network => "network",
            ErrorKind::Internal => "internal",
        }
    }
}

/// Plat5 standardized API error envelope.
///
/// Shape:
/// ```json
/// {
///   "error": {
///     "type": "invalid_request_error",
///     "code": "UPPER_SNAKE_CASE",
///     "message": "Human-readable description",
///     "request_id": "uuid-or-correlation-id",
///     "details": { ... } | null
///   }
/// }
/// ```
#[derive(Debug, Clone, Serialize)]
pub struct ApiError {
    pub error_type: String,
    pub code: String,
    pub message: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub details: Option<serde_json::Value>,
}

impl ApiError {
    pub fn invalid_request() -> Self {
        Self {
            error_type: "invalid_request_error".to_string(),
            code: "INVALID_REQUEST".to_string(),
            message: "Malformed request.".to_string(),
            details: None,
        }
    }

    pub fn unauthorized(details: Option<serde_json::Value>) -> Self {
        Self {
            error_type: "invalid_request_error".to_string(),
            code: "UNAUTHORIZED".to_string(),
            message: "Authentication required.".to_string(),
            details,
        }
    }

    /// A 401 on a call that takes any credential, such as route checks.
    pub fn unauthorized_any(details: Option<serde_json::Value>) -> Self {
        let mut err = Self::unauthorized(details);
        err.message = ANY_CREDENTIAL.to_string();
        err
    }

    /// A malformed body: **422**, with each field it got wrong.
    pub fn validation(fields: &[(String, String)]) -> Self {
        let message = fields
            .first()
            .map_or("That doesn't look right.", |(_, m)| m.as_str())
            .to_string();
        let fields: Vec<serde_json::Value> = fields
            .iter()
            .map(|(path, message)| serde_json::json!({ "path": path, "message": message }))
            .collect();
        Self {
            error_type: "invalid_request_error".to_string(),
            code: "VALIDATION_ERROR".to_string(),
            message,
            details: Some(serde_json::json!({ "fields": fields })),
        }
    }

    /// A 401 on a matched route: the message names what the route's scope takes.
    /// It never says why the credential sent failed, so it tells a caller nothing
    /// about a key they hold.
    pub fn unauthorized_for(scope: &Scope, details: Option<serde_json::Value>) -> Self {
        let mut err = Self::unauthorized(details);
        err.message = match scope {
            Scope::Kind(_) => ANY_CREDENTIAL,
            Scope::User => "Authentication required: a sign-in token or a personal API key.",
            Scope::Public => "Authentication required.",
        }
        .to_string();
        err
    }

    /// The one 403 body: a node that doesn't exist, a caller outside its tree, and a
    /// missing permission all answer this, with the route's `permissions` list.
    /// An `any_member` route has none: its 403 only means the caller isn't in
    /// the tree.
    pub fn forbidden(required: &Required) -> Self {
        let details = match required {
            Required::AnyOf(list) => Some(serde_json::json!({ "permissions": list })),
            Required::AnyMember | Required::Nothing => None,
        };
        Self {
            error_type: "invalid_request_error".to_string(),
            code: "FORBIDDEN".to_string(),
            message: "Permission denied, or it may not exist.".to_string(),
            details,
        }
    }

    /// A service account on a user route: the credential is good, and user
    /// routes are a person's own.
    pub fn service_account_forbidden() -> Self {
        Self {
            error_type: "invalid_request_error".to_string(),
            code: "FORBIDDEN".to_string(),
            message: "Service accounts can't use this route.".to_string(),
            details: None,
        }
    }

    /// A scoped or root-restricted personal API key or scoped JWT on a user route.
    pub fn scoped_credential_forbidden() -> Self {
        Self {
            error_type: "invalid_request_error".to_string(),
            code: "FORBIDDEN".to_string(),
            message: "Scoped credentials can't use this route.".to_string(),
            details: None,
        }
    }

    pub fn not_found() -> Self {
        Self {
            error_type: "invalid_request_error".to_string(),
            code: "NOT_FOUND".to_string(),
            message: "Resource not found.".to_string(),
            details: None,
        }
    }

    pub fn payload_too_large(max_size_bytes: u64) -> Self {
        Self {
            error_type: "invalid_request_error".to_string(),
            code: "PAYLOAD_TOO_LARGE".to_string(),
            message: "Request body is too large.".to_string(),
            details: Some(serde_json::json!({
                "max_size_bytes": max_size_bytes
            })),
        }
    }

    pub fn rate_limited(retry_after_seconds: u64) -> Self {
        Self {
            error_type: "api_error".to_string(),
            code: "RATE_LIMITED".to_string(),
            message: "Too many requests. Try again in a moment.".to_string(),
            details: Some(serde_json::json!({
                "retry_after_seconds": retry_after_seconds
            })),
        }
    }

    pub fn internal_error() -> Self {
        Self {
            error_type: "api_error".to_string(),
            code: "INTERNAL_ERROR".to_string(),
            message: "An unexpected error occurred.".to_string(),
            details: None,
        }
    }

    pub fn service_unavailable() -> Self {
        Self {
            error_type: "api_error".to_string(),
            code: "SERVICE_UNAVAILABLE".to_string(),
            message: "Service temporarily unavailable.".to_string(),
            details: None,
        }
    }

    pub fn to_json_bytes(&self, request_id: Option<&str>) -> Vec<u8> {
        let envelope = serde_json::json!({
            "error": {
                "type": self.error_type,
                "code": self.code,
                "message": self.message,
                "request_id": request_id,
                "details": self.details
            }
        });
        envelope.to_string().into_bytes()
    }
}

impl fmt::Display for ApiError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}: {}", self.code, self.message)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn envelope_shape_and_forbidden_details() {
        let err = ApiError::forbidden(&Required::AnyOf(vec!["project:read".to_string()]));
        let parsed: serde_json::Value =
            serde_json::from_slice(&err.to_json_bytes(Some("req-1"))).unwrap();
        assert_eq!(parsed["error"]["type"], "invalid_request_error");
        assert_eq!(parsed["error"]["code"], "FORBIDDEN");
        assert_eq!(parsed["error"]["request_id"], "req-1");
        assert_eq!(
            parsed["error"]["details"],
            serde_json::json!({ "permissions": ["project:read"] })
        );

        let any = ApiError::forbidden(&Required::AnyMember);
        let parsed: serde_json::Value = serde_json::from_slice(&any.to_json_bytes(None)).unwrap();
        assert!(parsed["error"]["details"].is_null());

        let val = ApiError::validation(&[
            ("checks[0].method".into(), "method must be GET.".into()),
            ("checks[1].path".into(), "path must start with /.".into()),
        ]);
        let parsed: serde_json::Value = serde_json::from_slice(&val.to_json_bytes(None)).unwrap();
        assert_eq!(parsed["error"]["code"], "VALIDATION_ERROR");
        assert_eq!(parsed["error"]["message"], "method must be GET.");
        assert_eq!(
            parsed["error"]["details"]["fields"]
                .as_array()
                .unwrap()
                .len(),
            2
        );
    }
}
