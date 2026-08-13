use axum::http::{HeaderValue, StatusCode};
use axum::response::{IntoResponse, Response};
use axum::Json;
use serde::Serialize;
use serde_json::{json, Value};

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
    pub details: Option<Value>,
    pub request_id: String,
}

impl AppError {
    pub fn unauthorized(request_id: String) -> Self {
        Self {
            status: StatusCode::UNAUTHORIZED,
            error_type: "invalid_request_error",
            code: "UNAUTHORIZED",
            message: "Authentication required".into(),
            details: Some(json!({ "reason": "invalid_or_missing_admin_token" })),
            request_id,
        }
    }

    pub fn not_found(request_id: String, resource: &str, id: &str) -> Self {
        Self {
            status: StatusCode::NOT_FOUND,
            error_type: "invalid_request_error",
            code: "NOT_FOUND",
            message: "Resource not found".into(),
            details: Some(json!({ "resource": resource, "id": id })),
            request_id,
        }
    }

    pub fn validation(request_id: String, message: String) -> Self {
        Self {
            status: StatusCode::UNPROCESSABLE_ENTITY,
            error_type: "invalid_request_error",
            code: "VALIDATION_ERROR",
            message: "Request validation failed".into(),
            details: Some(json!({ "fields": [{ "path": "body", "message": message }] })),
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
            message: "Service temporarily unavailable".into(),
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
                details: self.details,
            },
        };
        let mut res = (self.status, Json(body)).into_response();
        if let Ok(val) = HeaderValue::from_str(&request_id) {
            res.headers_mut().insert("x-request-id", val);
        }
        res
    }
}
