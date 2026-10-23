use axum::http::{HeaderValue, StatusCode};
use axum::response::{IntoResponse, Response};
use axum::Json;
use serde::Serialize;
use serde_json::{json, Value};

use crate::schema::FieldError;

#[derive(Debug, Clone, Serialize)]
pub struct ApiErrorBody {
    #[serde(rename = "type")]
    pub error_type: String,
    pub code: String,
    pub message: String,
    pub request_id: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub details: Option<Value>,
}

#[derive(Debug, Clone, Serialize)]
pub struct ApiErrorEnvelope {
    pub error: ApiErrorBody,
}

#[derive(Debug)]
pub struct AppError {
    pub status: StatusCode,
    pub error_type: &'static str,
    pub code: &'static str,
    pub message: String,
    /// Boxed: a `Value` makes every `Result<_, AppError>` large.
    pub details: Option<Box<Value>>,
    pub request_id: String,
}

impl AppError {
    pub fn unauthorized(request_id: String) -> Self {
        Self {
            status: StatusCode::UNAUTHORIZED,
            error_type: "invalid_request_error",
            code: "UNAUTHORIZED",
            message: "Authentication required.".into(),
            details: Some(Box::new(
                json!({ "reason": "invalid_or_missing_admin_token" }),
            )),
            request_id,
        }
    }

    pub fn unauthorized_internal(request_id: String) -> Self {
        Self {
            status: StatusCode::UNAUTHORIZED,
            error_type: "invalid_request_error",
            code: "UNAUTHORIZED",
            message: "Authentication required.".into(),
            details: Some(Box::new(json!({ "reason": "invalid_internal_token" }))),
            request_id,
        }
    }

    pub fn not_found(request_id: String, resource: &str, id: &str) -> Self {
        Self {
            status: StatusCode::NOT_FOUND,
            error_type: "invalid_request_error",
            code: "NOT_FOUND",
            message: "Resource not found.".into(),
            details: Some(Box::new(json!({ "resource": resource, "id": id }))),
            request_id,
        }
    }

    /// **422** naming each field path. The message is the first problem; the
    /// CLI adds the file to each path.
    pub fn validation(request_id: String, fields: Vec<FieldError>) -> Self {
        let message = match fields.as_slice() {
            [] => "That doesn't look right.".to_string(),
            [one] => one.to_string(),
            [first, rest @ ..] => format!("{first} (and {} more)", rest.len()),
        };
        Self {
            status: StatusCode::UNPROCESSABLE_ENTITY,
            error_type: "invalid_request_error",
            code: "VALIDATION_ERROR",
            message,
            details: Some(Box::new(json!({ "fields": fields }))),
            request_id,
        }
    }

    /// **409**: an apply's precondition no longer holds (docs/reference/errors.md#plan_stale).
    pub fn plan_stale(
        request_id: String,
        precondition: String,
        expected: Option<i64>,
        actual: Option<i64>,
    ) -> Self {
        Self {
            status: StatusCode::CONFLICT,
            error_type: "invalid_request_error",
            code: "PLAN_STALE",
            message: "The deployment changed since this plan was made. Plan again.".into(),
            details: Some(Box::new(
                json!({ "precondition": precondition, "expected": expected, "actual": actual }),
            )),
            request_id,
        }
    }

    /// **409**: each method and path shape owned by two services.
    pub fn route_conflict(request_id: String, conflicts: Vec<String>) -> Self {
        Self {
            status: StatusCode::CONFLICT,
            error_type: "invalid_request_error",
            code: "ROUTE_CONFLICT",
            message: format!("Route owned by two services: {}.", conflicts.join("; ")),
            details: None,
            request_id,
        }
    }

    pub fn invalid_request(request_id: String, message: impl Into<String>) -> Self {
        Self {
            status: StatusCode::BAD_REQUEST,
            error_type: "invalid_request_error",
            code: "INVALID_REQUEST",
            message: message.into(),
            details: None,
            request_id,
        }
    }

    pub fn service_unavailable(request_id: String) -> Self {
        Self {
            status: StatusCode::SERVICE_UNAVAILABLE,
            error_type: "api_error",
            code: "SERVICE_UNAVAILABLE",
            message: "Service temporarily unavailable.".into(),
            details: None,
            request_id,
        }
    }
}

impl IntoResponse for AppError {
    fn into_response(self) -> Response {
        let request_id = self.request_id.clone();
        let body = ApiErrorEnvelope {
            error: ApiErrorBody {
                error_type: self.error_type.to_string(),
                code: self.code.to_string(),
                message: self.message,
                request_id: request_id.clone(),
                details: self.details.map(|d| *d),
            },
        };
        let mut res = (self.status, Json(body)).into_response();
        if let Ok(val) = HeaderValue::from_str(&request_id) {
            res.headers_mut().insert("x-request-id", val);
        }
        res
    }
}
