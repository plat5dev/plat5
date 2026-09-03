use std::collections::HashMap;

use pingora::http::RequestHeader;
use tracing::{debug, warn};

use crate::auth::jwt::validate_token;
use crate::auth::member::{CachedMember, MemberError};
use crate::auth::member_apikey::{CachedMemberApiKey, MemberApiKeyError};
use crate::auth::user_apikey::{CachedUserApiKey, UserApiKeyError};
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
            key_scopes: auth.key_scopes,
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
            .filter(|k| k.starts_with(self.stack.member_key_prefix.as_str()))
            .map(|s| s.to_string());

        if let Some(key_str) = member_key {
            return self.admit_member_api_key(&organization_id, &key_str).await;
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
            key_scopes: auth.key_scopes,
        })
    }

    async fn admit_member_api_key(
        &self,
        path_organization_id: &str,
        key: &str,
    ) -> Result<Admission, AdmitError> {
        let cached = match self
            .stack
            .member_apikey_cache
            .get_or_load(key, async {
                match self.stack.member_apikey_validator.validate(key).await {
                    Ok(v) => {
                        let member_id = match v.member_id.clone() {
                            Some(id) if !id.is_empty() => id,
                            _ => {
                                warn!("member key validate returned valid without member_id");
                                return Err(MemberApiKeyError::ServiceError(
                                    "missing member_id".into(),
                                ));
                            }
                        };
                        let organization_id = match v.organization_id.clone() {
                            Some(id) if !id.is_empty() => id,
                            _ => {
                                warn!("member key validate returned valid without organization_id");
                                return Err(MemberApiKeyError::ServiceError(
                                    "missing organization_id".into(),
                                ));
                            }
                        };
                        Ok(CachedMemberApiKey::Valid {
                            member_id,
                            organization_id,
                            scopes: v.scopes.clone(),
                        })
                    }
                    Err(MemberApiKeyError::InvalidKey) => Ok(CachedMemberApiKey::Invalid),
                    Err(e) => Err(e),
                }
            })
            .await
        {
            Ok(v) => v,
            Err(err) => match err.as_ref() {
                MemberApiKeyError::InvalidKey => return Err(AdmitError::MemberApiKeyInvalid),
                MemberApiKeyError::ServiceError(msg) => {
                    warn!(
                        error_kind = ErrorKind::Network.as_str(),
                        error_message = %msg,
                        "member key validate error"
                    );
                    return Err(AdmitError::Unavailable);
                }
            },
        };

        match cached {
            CachedMemberApiKey::Invalid => Err(AdmitError::MemberApiKeyInvalid),
            CachedMemberApiKey::Valid {
                member_id,
                organization_id: key_org,
                scopes,
            } => {
                if key_org != path_organization_id {
                    debug!(
                        key_org = %key_org,
                        path_org = %path_organization_id,
                        "member key org mismatch"
                    );
                    return Err(AdmitError::NotFound);
                }
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
                    key_scopes: scopes,
                })
            }
        }
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
                        key_scopes: None,
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
                    key_scopes: None,
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

        if !key.starts_with(self.stack.user_key_prefix.as_str()) {
            return Err(AuthError::InvalidUserApiKey);
        }

        let cached = match self
            .stack
            .user_apikey_cache
            .get_or_load(key, async {
                match self.stack.user_apikey_validator.validate(key).await {
                    Ok(v) => {
                        let user_id = match v.user_id.clone() {
                            Some(id) if !id.is_empty() => id,
                            _ => {
                                warn!("user key validate returned valid without user_id");
                                return Err(UserApiKeyError::ServiceError(
                                    "missing user_id".into(),
                                ));
                            }
                        };
                        Ok(CachedUserApiKey::Valid {
                            user_id,
                            scopes: v.scopes.clone(),
                        })
                    }
                    Err(UserApiKeyError::InvalidKey) => Ok(CachedUserApiKey::Invalid),
                    Err(e) => Err(e),
                }
            })
            .await
        {
            Ok(v) => v,
            Err(err) => {
                return Err(match err.as_ref() {
                    UserApiKeyError::InvalidKey => AuthError::InvalidUserApiKey,
                    UserApiKeyError::ServiceError(msg) => {
                        warn!(
                            error_kind = ErrorKind::Network.as_str(),
                            error_message = %msg,
                            "user key validate error"
                        );
                        AuthError::UserApiKeyValidationUnavailable
                    }
                });
            }
        };

        match cached {
            CachedUserApiKey::Invalid => Err(AuthError::InvalidUserApiKey),
            CachedUserApiKey::Valid { user_id, scopes } => Ok(AuthContext {
                user_id,
                auth_type: AuthType::UserApiKey,
                kid: None,
                key_scopes: scopes,
            }),
        }
    }

    async fn resolve_active_member(
        &self,
        user_id: &str,
        organization_id: &str,
    ) -> Result<String, ResolveDeny> {
        let cached = match self
            .stack
            .member_cache
            .get_or_load(user_id, organization_id, async {
                match self
                    .stack
                    .member_resolver
                    .resolve(user_id, organization_id)
                    .await
                {
                    Ok(resolved) if resolved.status == "active" => {
                        Ok(CachedMember::Active(resolved.member_id))
                    }
                    Ok(_) | Err(MemberError::NotFound) => Ok(CachedMember::Miss),
                    Err(MemberError::ServiceError(_)) => Err(ResolveDeny::Unavailable),
                }
            })
            .await
        {
            Ok(v) => v,
            Err(_) => return Err(ResolveDeny::Unavailable),
        };

        match cached {
            CachedMember::Active(member_id) => Ok(member_id),
            CachedMember::Miss => Err(ResolveDeny::NotFound),
        }
    }
}
