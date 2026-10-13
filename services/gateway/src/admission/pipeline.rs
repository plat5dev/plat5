use pingora::http::RequestHeader;
use tracing::{debug, warn};

use crate::auth::credential::{parse_shape, Kind, Validated};
use crate::auth::jwt::validate_token;
use crate::auth::AuthStack;
use crate::error::ErrorKind;
use crate::route_map::{Route, RouteScope};

use super::types::{
    extract_claim_path, jwt_error_reason, key_display_prefix, Admission, AdmitError, AuthContext,
    AuthError, AuthType, RequestCredential,
};

/// Composes auth domains into route-scope admission decisions.
///
/// Every credential belongs to a membership in a realm (docs/gateway-contract.md#credentials).
/// A `user` route accepts the directory's: a JWT, or a user key. `organization` and
/// `member` routes accept an organization's: a member key or session. Another realm's
/// credential is 401.
pub struct Admissor {
    stack: AuthStack,
}

impl Admissor {
    pub fn new(stack: AuthStack) -> Self {
        Self { stack }
    }

    /// Admit `req` for `route`. Fills `cred` for the request log as far as it gets:
    /// the presented key's display prefix before validation, the principal after.
    pub async fn admit(
        &self,
        req: &RequestHeader,
        route: &Route,
        span: Option<&tracing::Span>,
        cred: &mut RequestCredential,
    ) -> Result<Admission, AdmitError> {
        match route.scope {
            RouteScope::Public => {
                debug!(path = %route.path, "skipping auth for public route");
                Ok(Admission::Public)
            }
            RouteScope::User => {
                let auth = match api_key(req)? {
                    Some(key) => match self.load_credential(&key, cred).await? {
                        Validated::Directory { user_id } => AuthContext {
                            user_id,
                            auth_type: AuthType::UserApiKey,
                            kid: None,
                        },
                        _ => return Err(AdmitError::WrongCredential),
                    },
                    None => self.check_jwt(req).await.map_err(AdmitError::Auth)?,
                };
                debug!(
                    auth_type = auth.auth_type.as_str(),
                    user_id = %auth.user_id,
                    "authentication successful"
                );
                cred.auth_type = Some(auth.auth_type);
                cred.user_id = Some(auth.user_id.clone());
                Ok(Admission::User {
                    user_id: auth.user_id,
                    auth_type: auth.auth_type,
                    kid: auth.kid,
                })
            }
            RouteScope::Organization | RouteScope::Member => {
                let Some(key) = api_key(req)? else {
                    return Err(AdmitError::WrongCredential);
                };
                let Validated::Organization {
                    member_id,
                    organization_id,
                    labels,
                    ..
                } = self.load_credential(&key, cred).await?
                else {
                    return Err(AdmitError::WrongCredential);
                };

                if let Some(span) = span {
                    span.record("organization.id", organization_id.as_str());
                    span.record("member.id", member_id.as_str());
                }
                cred.organization_id = Some(organization_id.clone());
                cred.member_id = Some(member_id.clone());
                debug!(
                    organization_id = %organization_id,
                    member_id = %member_id,
                    "member credential admitted"
                );

                // One organization result feeds both scopes. The scope picks the fields.
                Ok(match route.scope {
                    RouteScope::Organization => Admission::Organization {
                        organization_id,
                        labels,
                    },
                    _ => Admission::Member {
                        organization_id,
                        member_id,
                        labels,
                    },
                })
            }
        }
    }

    /// Shape-check an `X-API-Key`, then validate it (cached). A value of any other
    /// shape is never sent to identity. Valid and invalid results are cached alike.
    async fn load_credential(
        &self,
        key: &str,
        cred: &mut RequestCredential,
    ) -> Result<Validated, AdmitError> {
        let Some(shape) = parse_shape(key) else {
            return Err(AdmitError::CredentialInvalid);
        };
        cred.key_prefix = key_display_prefix(key, shape.wire_prefix_len);
        let metric = match shape.kind {
            Kind::Session => {
                cred.auth_type = Some(AuthType::MemberSession);
                AuthType::MemberSession.as_str()
            }
            Kind::Key => "apikey",
        };

        let validated = self
            .stack
            .credential_cache
            .get_or_load(key, self.stack.validator.credential(key, metric))
            .await
            .map_err(|err| {
                warn!(
                    error_kind = ErrorKind::Network.as_str(),
                    error_message = %err.0,
                    "credential validate error"
                );
                AdmitError::Unavailable
            })?;

        cred.auth_type = match &validated {
            Validated::Directory { .. } => Some(AuthType::UserApiKey),
            Validated::Organization { session: true, .. } => Some(AuthType::MemberSession),
            Validated::Organization { .. } => Some(AuthType::MemberApiKey),
            Validated::Invalid => return Err(AdmitError::CredentialInvalid),
        };
        Ok(validated)
    }

    /// Verify the JWT, then resolve its issuer and subject to the person (cached).
    async fn check_jwt(&self, req: &RequestHeader) -> Result<AuthContext, AuthError> {
        let authorization = req
            .headers
            .get("Authorization")
            .ok_or(AuthError::MissingAuthorization)?;

        let auth_value = authorization
            .to_str()
            .map_err(|_| AuthError::InvalidAuthorizationHeader)?;
        let mut parts = auth_value.split_whitespace();
        let token = match (parts.next(), parts.next()) {
            (Some("Bearer"), Some(token)) => token,
            _ => return Err(AuthError::InvalidAuthorizationHeader),
        };

        let (claims, kid) = match self.stack.jwt_cache.get(token).await {
            Some(cached) => {
                let kid = cached.header.kid.clone();
                (cached.claims, kid)
            }
            None => {
                let jwks = self
                    .stack
                    .jwt_validator
                    .get_jwks()
                    .await
                    .map_err(|_| AuthError::JwtValidationUnavailable)?;
                let (data, kid) = validate_token(
                    token,
                    self.stack.jwt_validator.get_issuer(),
                    jwks,
                    self.stack.jwt_validator.get_allowed_audiences().to_vec(),
                )
                .await
                .map_err(|e| AuthError::InvalidToken {
                    reason: jwt_error_reason(&e),
                })?;
                self.stack.jwt_cache.put(token, data.clone()).await;
                (data.claims, Some(kid))
            }
        };

        let subject = extract_claim_path(&claims, &self.stack.user_id_claim)
            .ok_or(AuthError::MissingUserId)?;
        // Validation checked `iss` against the configured issuer.
        let issuer = self.stack.jwt_validator.get_issuer();
        let resolved = self
            .stack
            .subject_cache
            .get_or_load(
                &issuer,
                &subject,
                self.stack.validator.idp(&issuer, &subject),
            )
            .await
            .map_err(|err| {
                warn!(
                    error_kind = ErrorKind::Network.as_str(),
                    error_message = %err.0,
                    "idp subject resolve error"
                );
                AuthError::SubjectResolveUnavailable
            })?;

        match resolved {
            Validated::Directory { user_id } => Ok(AuthContext {
                user_id,
                auth_type: AuthType::Jwt,
                kid,
            }),
            _ => Err(AuthError::InactiveUser),
        }
    }
}

/// `X-API-Key` present means it is the credential, including an empty value.
/// Do not fall through to `Authorization`.
fn api_key(req: &RequestHeader) -> Result<Option<String>, AdmitError> {
    match req.headers.get("X-API-Key") {
        None => Ok(None),
        Some(value) => {
            let key = value.to_str().map_err(|_| AdmitError::CredentialInvalid)?;
            Ok(Some(key.to_string()))
        }
    }
}
