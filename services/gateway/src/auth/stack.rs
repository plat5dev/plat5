use crate::auth::jwt::{JwtCache, JwtValidatorState};
use crate::auth::member::{MemberCache, MemberResolver};
use crate::auth::member_apikey::{MemberApiKeyCache, MemberApiKeyValidator};
use crate::auth::user_apikey::{UserApiKeyCache, UserApiKeyValidator};
use crate::config::GatewayConfig;
use crate::internal_http::InternalHttpClient;

const JWT_CACHE_CAPACITY: u64 = 10_000;
const JWT_CACHE_TTL_BUFFER_SECS: u64 = 60;
const USER_APIKEY_CACHE_CAPACITY: u64 = 10_000;
const MEMBER_APIKEY_CACHE_CAPACITY: u64 = 10_000;
const MEMBER_CACHE_CAPACITY: u64 = 10_000;

/// Wired auth backends + caches used by admission.
pub struct AuthStack {
    pub jwt_validator: JwtValidatorState,
    pub jwt_cache: JwtCache,
    pub user_id_claim: Vec<String>,
    pub user_key_prefix: String,
    pub member_key_prefix: String,
    pub user_apikey_validator: UserApiKeyValidator,
    pub user_apikey_cache: UserApiKeyCache,
    pub member_apikey_validator: Option<MemberApiKeyValidator>,
    pub member_apikey_cache: MemberApiKeyCache,
    pub member_resolver: Option<MemberResolver>,
    pub member_cache: MemberCache,
}

impl AuthStack {
    pub fn from_config(cfg: &GatewayConfig, jwt_validator: JwtValidatorState) -> Self {
        let http = InternalHttpClient::new(cfg.internal_auth_token.clone());

        let user_apikey_validator =
            UserApiKeyValidator::new(cfg.user_apikey_validate_url.clone(), http.clone());
        let member_apikey_validator = cfg
            .member_apikey_validate_url
            .clone()
            .map(|url| MemberApiKeyValidator::new(url, http.clone()));
        let member_resolver = cfg
            .member_resolve_url
            .clone()
            .map(|url| MemberResolver::new(url, http));

        Self {
            jwt_validator,
            jwt_cache: JwtCache::new(JWT_CACHE_CAPACITY, JWT_CACHE_TTL_BUFFER_SECS),
            user_id_claim: cfg.auth_user_id_claim.clone(),
            user_key_prefix: cfg.user_key_prefix.clone(),
            member_key_prefix: cfg.member_key_prefix.clone(),
            user_apikey_validator,
            user_apikey_cache: UserApiKeyCache::new(
                USER_APIKEY_CACHE_CAPACITY,
                cfg.apikey_cache_ttl_secs,
            ),
            member_apikey_validator,
            member_apikey_cache: MemberApiKeyCache::new(
                MEMBER_APIKEY_CACHE_CAPACITY,
                cfg.apikey_cache_ttl_secs,
            ),
            member_resolver,
            member_cache: MemberCache::new(MEMBER_CACHE_CAPACITY, cfg.member_cache_ttl_secs),
        }
    }
}
