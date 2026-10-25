use serde::Serialize;
use std::fmt;

use crate::route_map::{Required, Scope};

/// Standard error kinds for telemetry.
/// Used in span attributes and log fields.
/// Closed set: auth, network, db, io, internal, validation.
#[derive(Debug, Clone, Copy)]
pub enum ErrorKind {
    Auth,
    Network,
    Db,
    Io,
    Internal,
    Validation,
}

impl ErrorKind {
    pub fn as_str(&self) -> &'static str {
        match self {
            ErrorKind::Auth => "auth",
            ErrorKind::Network => "network",
            ErrorKind::Db => "db",
            ErrorKind::Io => "io",
            ErrorKind::Internal => "internal",
            ErrorKind::Validation => "validation",
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

    /// A 401 on a matched route: the message names what the route's scope takes
    /// (docs/reference/error-copy.md#gateway-401). It never says why the credential
    /// sent failed, so it tells a caller nothing about a key they hold.
    pub fn unauthorized_for(scope: &Scope, details: Option<serde_json::Value>) -> Self {
        let mut err = Self::unauthorized(details);
        err.message = match scope {
            Scope::Kind(_) => "Authentication required: a sign-in token or an API key.",
            Scope::User => "Authentication required: a sign-in token or a personal API key.",
            Scope::Public => "Authentication required.",
        }
        .to_string();
        err
    }

    /// The one 403 body: a node that doesn't exist, a caller outside its tree, and a
    /// missing permission all answer this, with the route's `required_permissions`:
    /// its list, or `"any_member"`.
    pub fn forbidden(required: &Required) -> Self {
        let required = match required {
            Required::AnyOf(list) => serde_json::json!(list),
            Required::AnyMember => serde_json::json!("any_member"),
            Required::Nothing => serde_json::Value::Null,
        };
        Self {
            error_type: "invalid_request_error".to_string(),
            code: "FORBIDDEN".to_string(),
            message: "Permission denied, or it may not exist.".to_string(),
            details: Some(serde_json::json!({ "required_permissions": required })),
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
    fn test_invalid_request_serialization() {
        let err = ApiError::invalid_request();
        let json = String::from_utf8(err.to_json_bytes(None)).unwrap();
        let parsed: serde_json::Value = serde_json::from_str(&json).unwrap();
        assert_eq!(parsed["error"]["code"], "INVALID_REQUEST");
        assert_eq!(parsed["error"]["message"], "Malformed request.");
    }

    #[test]
    fn test_unauthorized_names_what_the_scope_takes() {
        let user = ApiError::unauthorized_for(&Scope::User, None);
        assert_eq!(
            user.message,
            "Authentication required: a sign-in token or a personal API key."
        );
        let kind = ApiError::unauthorized_for(&Scope::Kind("project".into()), None);
        assert_eq!(
            kind.message,
            "Authentication required: a sign-in token or an API key."
        );
        assert_eq!(kind.code, "UNAUTHORIZED");
    }

    #[test]
    fn test_unauthorized_serialization() {
        let err = ApiError::unauthorized(Some(serde_json::json!({"reason": "token_expired"})));
        let json = String::from_utf8(err.to_json_bytes(Some("req-123"))).unwrap();
        let parsed: serde_json::Value = serde_json::from_str(&json).unwrap();
        assert_eq!(parsed["error"]["type"], "invalid_request_error");
        assert_eq!(parsed["error"]["code"], "UNAUTHORIZED");
        assert_eq!(parsed["error"]["message"], "Authentication required.");
        assert_eq!(parsed["error"]["request_id"], "req-123");
        assert_eq!(parsed["error"]["details"]["reason"], "token_expired");
    }

    #[test]
    fn test_not_found_serialization() {
        let err = ApiError::not_found();
        let json = String::from_utf8(err.to_json_bytes(None)).unwrap();
        let parsed: serde_json::Value = serde_json::from_str(&json).unwrap();
        assert_eq!(parsed["error"]["type"], "invalid_request_error");
        assert_eq!(parsed["error"]["code"], "NOT_FOUND");
        assert_eq!(parsed["error"]["message"], "Resource not found.");
        assert!(parsed["error"]["details"].is_null());
    }

    #[test]
    fn test_payload_too_large_serialization() {
        let err = ApiError::payload_too_large(10_485_760);
        let json = String::from_utf8(err.to_json_bytes(None)).unwrap();
        let parsed: serde_json::Value = serde_json::from_str(&json).unwrap();
        assert_eq!(parsed["error"]["type"], "invalid_request_error");
        assert_eq!(parsed["error"]["code"], "PAYLOAD_TOO_LARGE");
        assert_eq!(parsed["error"]["details"]["max_size_bytes"], 10_485_760);
    }

    #[test]
    fn test_internal_error_serialization() {
        let err = ApiError::internal_error();
        let json = String::from_utf8(err.to_json_bytes(Some("req-456"))).unwrap();
        let parsed: serde_json::Value = serde_json::from_str(&json).unwrap();
        assert_eq!(parsed["error"]["type"], "api_error");
        assert_eq!(parsed["error"]["code"], "INTERNAL_ERROR");
        assert_eq!(parsed["error"]["message"], "An unexpected error occurred.");
        assert_eq!(parsed["error"]["request_id"], "req-456");
        assert!(parsed["error"]["details"].is_null());
    }

    #[test]
    fn test_service_unavailable_serialization() {
        let err = ApiError::service_unavailable();
        let json = String::from_utf8(err.to_json_bytes(None)).unwrap();
        let parsed: serde_json::Value = serde_json::from_str(&json).unwrap();
        assert_eq!(parsed["error"]["type"], "api_error");
        assert_eq!(parsed["error"]["code"], "SERVICE_UNAVAILABLE");
        assert_eq!(
            parsed["error"]["message"],
            "Service temporarily unavailable."
        );
    }

    #[test]
    fn test_forbidden_serialization() {
        let err = ApiError::forbidden(&Required::AnyOf(vec!["project:read".to_string()]));
        let json = String::from_utf8(err.to_json_bytes(None)).unwrap();
        let parsed: serde_json::Value = serde_json::from_str(&json).unwrap();
        assert_eq!(parsed["error"]["type"], "invalid_request_error");
        assert_eq!(parsed["error"]["code"], "FORBIDDEN");
        assert_eq!(
            parsed["error"]["message"],
            "Permission denied, or it may not exist."
        );
        assert_eq!(
            parsed["error"]["details"],
            serde_json::json!({ "required_permissions": ["project:read"] })
        );
        let any = ApiError::forbidden(&Required::AnyMember);
        assert_eq!(
            any.details,
            Some(serde_json::json!({ "required_permissions": "any_member" }))
        );
    }

    #[test]
    fn test_rate_limited_serialization() {
        let err = ApiError::rate_limited(12);
        let json = String::from_utf8(err.to_json_bytes(None)).unwrap();
        let parsed: serde_json::Value = serde_json::from_str(&json).unwrap();
        assert_eq!(parsed["error"]["type"], "api_error");
        assert_eq!(parsed["error"]["code"], "RATE_LIMITED");
        assert_eq!(
            parsed["error"]["message"],
            "Too many requests. Try again in a moment."
        );
        assert_eq!(parsed["error"]["details"]["retry_after_seconds"], 12);
    }

    #[test]
    fn test_display() {
        let err = ApiError::not_found();
        assert_eq!(format!("{err}"), "NOT_FOUND: Resource not found.");
    }

    #[test]
    fn test_error_kind_as_str() {
        assert_eq!(ErrorKind::Auth.as_str(), "auth");
        assert_eq!(ErrorKind::Network.as_str(), "network");
        assert_eq!(ErrorKind::Db.as_str(), "db");
        assert_eq!(ErrorKind::Io.as_str(), "io");
        assert_eq!(ErrorKind::Internal.as_str(), "internal");
        assert_eq!(ErrorKind::Validation.as_str(), "validation");
    }
}
