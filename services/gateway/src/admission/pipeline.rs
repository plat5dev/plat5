use pingora::http::RequestHeader;
use tracing::{debug, warn};

use crate::auth::jwt::validate_token;
use crate::auth::member_apikey::{CachedMemberApiKey, MemberApiKeyError};
use crate::auth::member_session::{CachedMemberSession, MemberSessionError};
use crate::auth::user_apikey::{CachedUserApiKey, UserApiKeyError};
use crate::auth::AuthStack;
use crate::error::ErrorKind;
use crate::route_map::{Route, RouteScope};

use super::types::{
    extract_claim_path, jwt_error_reason, Admission, AdmitError, AuthContext, AuthError, AuthType,
};

/// Member key or member session, before the scope drops fields.
struct MemberProof {
    member_id: String,
    organization_id: String,
    key_scopes: Option<Vec<String>>,
}

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
        span: Option<&tracing::Span>,
    ) -> Result<Admission, AdmitError> {
        match route.scope {
            RouteScope::Public => {
                debug!(path = %route.path, "skipping auth for public route");
                Ok(Admission::Public)
            }
            RouteScope::User => self.admit_user(req).await,
            RouteScope::Organization | RouteScope::Member => {
                self.admit_member_credential(req, route.scope, span).await
            }
        }
    }

    async fn admit_user(&self, req: &RequestHeader) -> Result<Admission, AdmitError> {
        if let Some(key) = api_key(req)? {
            if !key.starts_with(self.stack.user_key_prefix.as_str()) {
                return Err(AdmitError::WrongCredential);
            }
            let auth = self.check_user_api_key(&key).await?;
            debug!(
                auth_type = auth.auth_type.as_str(),
                user_id = %auth.user_id,
                "authentication successful"
            );
            return Ok(Admission::User {
                user_id: auth.user_id,
                auth_type: auth.auth_type,
                kid: auth.kid,
                key_scopes: auth.key_scopes,
            });
        }

        let auth = self.check_jwt(req).await.map_err(AdmitError::Auth)?;
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

    async fn admit_member_credential(
        &self,
        req: &RequestHeader,
        scope: RouteScope,
        span: Option<&tracing::Span>,
    ) -> Result<Admission, AdmitError> {
        let Some(key) = api_key(req)? else {
            return Err(AdmitError::WrongCredential);
        };

        let proof = if key.starts_with(self.stack.member_key_prefix.as_str()) {
            self.load_member_key(&key).await?
        } else if key.starts_with(self.stack.session_prefix.as_str()) {
            self.load_member_session(&key).await?
        } else {
            return Err(AdmitError::WrongCredential);
        };

        if let Some(span) = span {
            span.record("organization.id", proof.organization_id.as_str());
            span.record("member.id", proof.member_id.as_str());
        }

        debug!(
            organization_id = %proof.organization_id,
            member_id = %proof.member_id,
            "member credential admitted"
        );

        match scope {
            RouteScope::Organization => Ok(Admission::Organization {
                organization_id: proof.organization_id,
                key_scopes: proof.key_scopes,
            }),
            RouteScope::Member => Ok(Admission::Member {
                organization_id: proof.organization_id,
                member_id: proof.member_id,
                key_scopes: proof.key_scopes,
            }),
            RouteScope::Public | RouteScope::User => Err(AdmitError::Internal(
                "member credential on a user subject route",
            )),
        }
    }

    async fn load_member_key(&self, key: &str) -> Result<MemberProof, AdmitError> {
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
                organization_id,
                scopes,
            } => Ok(MemberProof {
                member_id,
                organization_id,
                key_scopes: scopes,
            }),
        }
    }

    async fn load_member_session(&self, token: &str) -> Result<MemberProof, AdmitError> {
        let cached = match self
            .stack
            .member_session_cache
            .get_or_load(token, async {
                match self.stack.member_session_validator.validate(token).await {
                    Ok(v) => {
                        let member_id = match v.member_id.clone() {
                            Some(id) if !id.is_empty() => id,
                            _ => {
                                warn!("member session validate returned valid without member_id");
                                return Err(MemberSessionError::ServiceError(
                                    "missing member_id".into(),
                                ));
                            }
                        };
                        let organization_id = match v.organization_id.clone() {
                            Some(id) if !id.is_empty() => id,
                            _ => {
                                warn!(
                                    "member session validate returned valid without organization_id"
                                );
                                return Err(MemberSessionError::ServiceError(
                                    "missing organization_id".into(),
                                ));
                            }
                        };
                        Ok(CachedMemberSession::Valid {
                            member_id,
                            organization_id,
                            scopes: v.scopes.clone(),
                        })
                    }
                    Err(MemberSessionError::InvalidToken) => Ok(CachedMemberSession::Invalid),
                    Err(e) => Err(e),
                }
            })
            .await
        {
            Ok(v) => v,
            Err(err) => match err.as_ref() {
                MemberSessionError::InvalidToken => return Err(AdmitError::MemberSessionInvalid),
                MemberSessionError::ServiceError(msg) => {
                    warn!(
                        error_kind = ErrorKind::Network.as_str(),
                        error_message = %msg,
                        "member session validate error"
                    );
                    return Err(AdmitError::Unavailable);
                }
            },
        };

        match cached {
            CachedMemberSession::Invalid => Err(AdmitError::MemberSessionInvalid),
            CachedMemberSession::Valid {
                member_id,
                organization_id,
                scopes,
            } => Ok(MemberProof {
                member_id,
                organization_id,
                key_scopes: scopes,
            }),
        }
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

    async fn check_user_api_key(&self, key: &str) -> Result<AuthContext, AdmitError> {
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
                    UserApiKeyError::InvalidKey => AdmitError::Auth(AuthError::InvalidUserApiKey),
                    UserApiKeyError::ServiceError(msg) => {
                        warn!(
                            error_kind = ErrorKind::Network.as_str(),
                            error_message = %msg,
                            "user key validate error"
                        );
                        AdmitError::Auth(AuthError::UserApiKeyValidationUnavailable)
                    }
                });
            }
        };

        match cached {
            CachedUserApiKey::Invalid => Err(AdmitError::Auth(AuthError::InvalidUserApiKey)),
            CachedUserApiKey::Valid { user_id, scopes } => Ok(AuthContext {
                user_id,
                auth_type: AuthType::UserApiKey,
                kid: None,
                key_scopes: scopes,
            }),
        }
    }
}

/// `X-API-Key` present means it is the credential, including an empty value.
/// Do not fall through to `Authorization`.
fn api_key(req: &RequestHeader) -> Result<Option<String>, AdmitError> {
    match req.headers.get("X-API-Key") {
        None => Ok(None),
        Some(value) => {
            let key = value.to_str().map_err(|_| AdmitError::WrongCredential)?;
            Ok(Some(key.to_string()))
        }
    }
}
