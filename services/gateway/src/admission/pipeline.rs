use std::collections::HashMap;

use pingora::http::RequestHeader;
use tracing::{debug, warn};

use crate::auth::jwt::validate_token;
use crate::auth::member::MemberError;
use crate::auth::member_apikey::{MemberApiKeyError, MEMBER_KEY_PREFIX};
use crate::auth::user_apikey::{UserApiKeyError, USER_KEY_PREFIX};
use crate::auth::AuthStack;
use crate::error::ErrorKind;
use crate::route_map::{Route, RouteScope};

use super::types::{
    extract_claim_path, jwt_error_reason, organization_id_from_params, Admission, AdmitError,
    AuthContext, AuthError, AuthType, OrgParamError, OrgVia, ResolveDeny,
};

/// Composes auth domains into route-scope admission decisions.
pub struct Admissor {
    stack: AuthStack,
}

impl Admissor {
    pub fn new(stack: AuthStack) -> Self {
        Self { stack }
    }

    pub async fn admit(
        &self,
        req: &RequestHeader,
        route: &Route,
        params: &HashMap<String, String>,
    ) -> Result<Admission, AdmitError> {
        match route.scope {
            RouteScope::Public => {
                debug!(path = %route.path, "skipping auth for public route");
                Ok(Admission::Public)
            }
            RouteScope::User => self.admit_user(req).await,
            RouteScope::Organization => self.admit_organization(req, route, params).await,
        }
    }

    async fn admit_user(&self, req: &RequestHeader) -> Result<Admission, AdmitError> {
        let auth = self.authenticate(req).await.map_err(AdmitError::Auth)?;
        debug!(
            auth_type = auth.auth_type.as_str(),
            user_id = %auth.user_id,
            "authentication successful"
        );
        Ok(Admission::User {
            user_id: auth.user_id,
            auth_type: auth.auth_type,
            kid: auth.kid,
        })
    }

    async fn admit_organization(
        &self,
        req: &RequestHeader,
        route: &Route,
        params: &HashMap<String, String>,
    ) -> Result<Admission, AdmitError> {
        let organization_id =
            match organization_id_from_params(route.organization_param.as_deref(), params) {
                Ok(id) => id,
                Err(OrgParamError::MissingParamName) => {
                    return Err(AdmitError::Internal(
                        "organization route missing organization_param",
                    ));
                }
                Err(OrgParamError::MissingParamValue { .. }) => {
                    return Err(AdmitError::Internal(
                        "organization id missing from path params",
                    ));
                }
            };

        let member_key = req
            .headers
            .get("X-API-Key")
            .and_then(|v| v.to_str().ok())
            .filter(|k| k.starts_with(MEMBER_KEY_PREFIX))
            .map(|s| s.to_string());

        if let Some(key_str) = member_key {
            return self
                .admit_member_api_key(&organization_id, &key_str)
                .await;
        }

        let auth = self.authenticate(req).await.map_err(AdmitError::Auth)?;

        let member_id = self
            .resolve_active_member(&auth.user_id, &organization_id)
            .await
            .map_err(|d| match d {
                ResolveDeny::NotFound => {
                    debug!(
                        user_id = %auth.user_id,
                        organization_id = %organization_id,
                        "member resolve miss or inactive"
                    );
                    AdmitError::NotFound
                }
                ResolveDeny::Unavailable => {
                    warn!(
                        user_id = %auth.user_id,
                        organization_id = %organization_id,
                        "member resolve unavailable"
                    );
                    AdmitError::Unavailable
                }
            })?;

        debug!(
            auth_type = auth.auth_type.as_str(),
            user_id = %auth.user_id,
            organization_id = %organization_id,
            member_id = %member_id,
            "organization admission successful"
        );

        Ok(Admission::Organization {
            organization_id,
            member_id,
            via: OrgVia::User {
                user_id: auth.user_id,
                auth_type: auth.auth_type,
                kid: auth.kid,
            },
        })
    }

    async fn admit_member_api_key(
        &self,
        path_organization_id: &str,
        key: &str,
    ) -> Result<Admission, AdmitError> {
        if let Some(cached) = self.stack.member_apikey_cache.get(key).await {
            if cached.organization_id != path_organization_id {
                debug!(
                    key_org = %cached.organization_id,
                    path_org = %path_organization_id,
                    "member key org mismatch"
                );
                return Err(AdmitError::NotFound);
            }
            debug!(
                auth_type = AuthType::MemberApiKey.as_str(),
                organization_id = %path_organization_id,
                member_id = %cached.member_id,
                "organization admission successful"
            );
            return Ok(Admission::Organization {
                organization_id: path_organization_id.to_string(),
                member_id: cached.member_id,
                via: OrgVia::MemberKey,
            });
        }

        let validator = self.stack.member_apikey_validator.as_ref().ok_or_else(|| {
            warn!("member api key presented but MEMBER_APIKEY_VALIDATE_URL unset");
            AdmitError::Unavailable
        })?;

        let validation = match validator.validate(key).await {
            Ok(v) => v,
            Err(MemberApiKeyError::InvalidKey) => return Err(AdmitError::MemberApiKeyInvalid),
            Err(MemberApiKeyError::ServiceError(msg)) => {
                warn!(
                    error_kind = ErrorKind::Network.as_str(),
                    error_message = %msg,
                    "member key validate error"
                );
                return Err(AdmitError::Unavailable);
            }
        };

        let member_id = match validation.member_id.clone() {
            Some(id) if !id.is_empty() => id,
            _ => {
                warn!("member key validate returned valid without member_id");
                return Err(AdmitError::Unavailable);
            }
        };
        let key_org = match validation.organization_id.clone() {
            Some(id) if !id.is_empty() => id,
            _ => {
                warn!("member key validate returned valid without organization_id");
                return Err(AdmitError::Unavailable);
            }
        };

        if key_org != path_organization_id {
            debug!(
                key_org = %key_org,
                path_org = %path_organization_id,
                "member key org mismatch"
            );
            return Err(AdmitError::NotFound);
        }

        self.stack
            .member_apikey_cache
            .put(key, member_id.clone(), key_org)
            .await;

        debug!(
            auth_type = AuthType::MemberApiKey.as_str(),
            organization_id = %path_organization_id,
            member_id = %member_id,
            "organization admission successful"
        );

        Ok(Admission::Organization {
            organization_id: path_organization_id.to_string(),
            member_id,
            via: OrgVia::MemberKey,
        })
    }

    async fn authenticate(&self, req: &RequestHeader) -> Result<AuthContext, AuthError> {
        if req.headers.contains_key("X-API-Key") {
            return self.check_user_api_key(req).await;
        }
        self.check_jwt(req).await
    }

    async fn check_jwt(&self, req: &RequestHeader) -> Result<AuthContext, AuthError> {
        let authorization = req
            .headers
            .get("Authorization")
            .ok_or(AuthError::MissingAuthorization)?;

        let auth_value = authorization
            .to_str()
            .map_err(|_| AuthError::InvalidAuthorizationHeader)?;
        let mut parts = auth_value.split_whitespace();
        match (parts.next(), parts.next()) {
            (Some("Bearer"), Some(token)) => {
                if let Some(cached_claims) = self.stack.jwt_cache.get(token).await {
                    let user_id =
                        extract_claim_path(&cached_claims.claims, &self.stack.user_id_claim)
                            .ok_or(AuthError::MissingUserId)?;
                    return Ok(AuthContext {
                        user_id,
                        auth_type: AuthType::Jwt,
                        kid: cached_claims.header.kid.clone(),
                    });
                }

                let jwks = self
                    .stack
                    .jwt_validator
                    .get_jwks()
                    .await
                    .map_err(|_| AuthError::JwtValidationUnavailable)?;
                let (claims, kid) = validate_token(
                    token,
                    self.stack.jwt_validator.get_issuer(),
                    jwks,
                    self.stack.jwt_validator.get_allowed_audiences().to_vec(),
                )
                .await
                .map_err(|e| AuthError::InvalidToken {
                    reason: jwt_error_reason(&e),
                })?;

                self.stack.jwt_cache.put(token, claims.clone()).await;

                let user_id = extract_claim_path(&claims.claims, &self.stack.user_id_claim)
                    .ok_or(AuthError::MissingUserId)?;
                Ok(AuthContext {
                    user_id,
                    auth_type: AuthType::Jwt,
                    kid: Some(kid),
                })
            }
            _ => Err(AuthError::InvalidAuthorizationHeader),
        }
    }

    async fn check_user_api_key(&self, req: &RequestHeader) -> Result<AuthContext, AuthError> {
        let api_key = req
            .headers
            .get("X-API-Key")
            .ok_or(AuthError::MissingUserApiKey)?;

        let key = api_key
            .to_str()
            .map_err(|_| AuthError::InvalidUserApiKeyHeader)?;

        if !key.starts_with(USER_KEY_PREFIX) {
            return Err(AuthError::InvalidUserApiKey);
        }

        if let Some(user_id) = self.stack.user_apikey_cache.get(key).await {
            return Ok(AuthContext {
                user_id,
                auth_type: AuthType::UserApiKey,
                kid: None,
            });
        }

        let validation = self
            .stack
            .user_apikey_validator
            .validate(key)
            .await
            .map_err(|e| match e {
                UserApiKeyError::InvalidKey => AuthError::InvalidUserApiKey,
                UserApiKeyError::ServiceError(msg) => {
                    warn!(
                        error_kind = ErrorKind::Network.as_str(),
                        error_message = %msg,
                        "user key validate error"
                    );
                    AuthError::UserApiKeyValidationUnavailable
                }
            })?;

        let user_id = validation.user_id.clone().ok_or_else(|| {
            warn!("user key validate returned valid without user_id");
            AuthError::UserApiKeyValidationUnavailable
        })?;

        self.stack
            .user_apikey_cache
            .put(key, user_id.clone())
            .await;

        Ok(AuthContext {
            user_id,
            auth_type: AuthType::UserApiKey,
            kid: None,
        })
    }

    async fn resolve_active_member(
        &self,
        user_id: &str,
        organization_id: &str,
    ) -> Result<String, ResolveDeny> {
        if let Some(member_id) = self.stack.member_cache.get(user_id, organization_id).await {
            return Ok(member_id);
        }

        let resolver = self
            .stack
            .member_resolver
            .as_ref()
            .ok_or(ResolveDeny::Unavailable)?;

        match resolver.resolve(user_id, organization_id).await {
            Ok(resolved) => {
                if resolved.status != "active" {
                    return Err(ResolveDeny::NotFound);
                }
                let member_id = resolved.member_id;
                self.stack
                    .member_cache
                    .put(user_id, organization_id, member_id.clone())
                    .await;
                Ok(member_id)
            }
            Err(MemberError::NotFound) => Err(ResolveDeny::NotFound),
            Err(MemberError::ServiceError(_)) => Err(ResolveDeny::Unavailable),
        }
    }
}
