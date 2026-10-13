//! Every Plat5 credential: API keys and sessions on `X-API-Key`, and the person a
//! JWT's subject resolves to. One identity validate, one cache per question.

mod shape;
mod validator;

use std::sync::Arc;

use crate::auth::cache::TtlCache;

pub use shape::{parse as parse_shape, Kind, Shape};
pub use validator::{CredentialValidator, ValidateError, Validated};

/// Caches validate results by the credential's hash, valid and invalid alike.
#[derive(Clone)]
pub struct CredentialCache {
    inner: TtlCache<Validated>,
}

impl CredentialCache {
    pub fn new(capacity: u64, ttl_secs: u64) -> Self {
        Self {
            inner: TtlCache::new(capacity, ttl_secs, "credential"),
        }
    }

    pub async fn get_or_load<Fut>(
        &self,
        value: &str,
        init: Fut,
    ) -> Result<Validated, Arc<ValidateError>>
    where
        Fut: std::future::Future<Output = Result<Validated, ValidateError>>,
    {
        self.inner.try_get_with_secret(value, init).await
    }
}

/// Caches an IdP issuer and subject to the person, or to `Invalid` for a suspended
/// person. Not secrets, so not hashed.
#[derive(Clone)]
pub struct SubjectCache {
    inner: TtlCache<Validated>,
}

impl SubjectCache {
    pub fn new(capacity: u64, ttl_secs: u64) -> Self {
        Self {
            inner: TtlCache::new(capacity, ttl_secs, "idp"),
        }
    }

    pub async fn get_or_load<Fut>(
        &self,
        issuer: &str,
        subject: &str,
        init: Fut,
    ) -> Result<Validated, Arc<ValidateError>>
    where
        Fut: std::future::Future<Output = Result<Validated, ValidateError>>,
    {
        // A NUL separates them: neither an issuer URL nor a subject contains one.
        self.inner
            .try_get_with(format!("{issuer}\u{0}{subject}"), init)
            .await
    }
}
