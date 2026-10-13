use crate::auth::credential::{CredentialCache, CredentialValidator, SubjectCache};
use crate::auth::jwt::{JwtCache, JwtValidatorState};
use crate::config::GatewayConfig;
use crate::internal_http::InternalHttpClient;

const JWT_CACHE_CAPACITY: u64 = 10_000;
const JWT_CACHE_TTL_BUFFER_SECS: u64 = 60;
const CREDENTIAL_CACHE_CAPACITY: u64 = 30_000;
const SUBJECT_CACHE_CAPACITY: u64 = 10_000;

/// Wired auth backends + caches used by admission.
pub struct AuthStack {
    pub jwt_validator: JwtValidatorState,
    pub jwt_cache: JwtCache,
    pub user_id_claim: Vec<String>,
    pub validator: CredentialValidator,
    pub credential_cache: CredentialCache,
    pub subject_cache: SubjectCache,
}

impl AuthStack {
    pub fn from_config(cfg: &GatewayConfig, jwt_validator: JwtValidatorState) -> Self {
        let http = InternalHttpClient::new(cfg.internal_auth_token.clone());
        Self {
            jwt_validator,
            jwt_cache: JwtCache::new(JWT_CACHE_CAPACITY, JWT_CACHE_TTL_BUFFER_SECS),
            user_id_claim: cfg.auth_user_id_claim.clone(),
            validator: CredentialValidator::new(cfg.credential_validate_url.clone(), http),
            credential_cache: CredentialCache::new(
                CREDENTIAL_CACHE_CAPACITY,
                cfg.apikey_cache_ttl_secs,
            ),
            subject_cache: SubjectCache::new(SUBJECT_CACHE_CAPACITY, cfg.apikey_cache_ttl_secs),
        }
    }
}
