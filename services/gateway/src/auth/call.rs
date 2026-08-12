use std::time::Instant;

use tracing::warn;

use crate::error::ErrorKind;
use crate::internal_http::InternalHttpError;
use crate::metrics;

/// Times an identity internal call and records `auth_validation_*` metrics.
pub struct AuthCallTimer {
    auth_type: &'static str,
    start: Instant,
}

impl AuthCallTimer {
    pub fn start(auth_type: &'static str) -> Self {
        Self {
            auth_type,
            start: Instant::now(),
        }
    }

    fn elapsed(&self) -> f64 {
        self.start.elapsed().as_secs_f64()
    }

    pub fn finish(self, outcome: &str) {
        metrics::record_auth_validation(self.auth_type, outcome, self.elapsed());
    }

    /// Record transport failure metrics + warn log. Returns a service-error message.
    pub fn finish_transport(self, err: InternalHttpError, op: &str) -> String {
        metrics::record_auth_validation(self.auth_type, "error", self.elapsed());
        match err {
            InternalHttpError::Network(msg) => {
                warn!(
                    error_kind = ErrorKind::Network.as_str(),
                    error_message = %msg,
                    "{op} failed"
                );
                msg
            }
            InternalHttpError::HttpStatus { status } => {
                warn!(
                    error_kind = ErrorKind::Network.as_str(),
                    status, "{op} returned error status"
                );
                format!("{op} returned status {status}")
            }
            InternalHttpError::Decode(msg) => {
                warn!(
                    error_kind = ErrorKind::Internal.as_str(),
                    error_message = %msg,
                    "failed to parse {op} response"
                );
                msg
            }
        }
    }
}
